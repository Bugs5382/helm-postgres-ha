package agent

/*
MIT License

Copyright (c) 2026 Shane

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
*/

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	golog "github.com/Bugs5382/go-log"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
	"github.com/Bugs5382/helm-postgres-ha/internal/config"
	"github.com/Bugs5382/helm-postgres-ha/internal/kube"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/metrics"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
)

// fakeNode simulates a PostgreSQL server and its data directory.
type fakeNode struct {
	mu         sync.Mutex
	hasData    bool
	running    bool
	inRecovery bool
	timeline   int
	lsn        uint64
	streaming  bool
	sysid      string
	cloneSysid string
	signals    map[string]bool
	conf       map[string]string
	stops      []pg.StopMode
	starts     int
	reloads    int
	promoteOK  bool
	rewindErr  error
	rewinds    int
	clones     int
	asides     int
	roles      int
	slots      map[string]bool
	lostSlots  map[string]bool
	dropped    []string
	rewindNoop bool
	reps       []pg.Replica
	// onStop runs inside Stop while the server is still up, to simulate a
	// slow shutdown.
	onStop func()
}

func newNode() *fakeNode {
	return &fakeNode{signals: map[string]bool{}, slots: map[string]bool{}, promoteOK: true, timeline: 1, sysid: "100", cloneSysid: "100"}
}

func (f *fakeNode) lock() func() { f.mu.Lock(); return f.mu.Unlock }

func (f *fakeNode) HasData() (bool, error)                   { defer f.lock()(); return f.hasData, nil }
func (f *fakeNode) DataMajor() (int, error)                  { return 18, nil }
func (f *fakeNode) ServerMajor(context.Context) (int, error) { return 18, nil }
func (f *fakeNode) Control(context.Context) (pg.Control, error) {
	defer f.lock()()
	return pg.Control{SystemID: f.sysid, Timeline: f.timeline}, nil
}
func (f *fakeNode) HasSignal(n string) bool { defer f.lock()(); return f.signals[n] }
func (f *fakeNode) SetSignal(n string, on bool) error {
	defer f.lock()()
	f.signals[n] = on
	return nil
}
func (f *fakeNode) WriteAgentConf(s map[string]string) (bool, error) {
	defer f.lock()()
	changed := len(s) != len(f.conf)
	for k, v := range s {
		if f.conf[k] != v {
			changed = true
		}
	}
	f.conf = s
	return changed, nil
}
func (f *fakeNode) WritePassFile([]pg.PassEntry) error { return nil }
func (f *fakeNode) Start() error {
	defer f.lock()()
	f.running = true
	f.starts++
	f.inRecovery = f.signals[pg.StandbySignal] || f.signals[pg.RecoverySignal]
	return nil
}
func (f *fakeNode) Running() bool { defer f.lock()(); return f.running }
func (f *fakeNode) Stop(_ context.Context, m pg.StopMode) error {
	if f.onStop != nil {
		f.onStop()
	}
	defer f.lock()()
	if f.running {
		f.stops = append(f.stops, m)
	}
	f.running, f.streaming = false, false
	return nil
}
func (f *fakeNode) Reload() error              { defer f.lock()(); f.reloads++; return nil }
func (f *fakeNode) Ping(context.Context) error { return nil }
func (f *fakeNode) Status(context.Context) (pg.Status, error) {
	defer f.lock()()
	if !f.running {
		return pg.Status{}, pg.ErrNotRunning
	}
	s := pg.Status{InRecovery: f.inRecovery, SystemID: f.sysid, Timeline: f.timeline}
	if f.inRecovery {
		s.ReceiveLSN, s.ReplayLSN = pg.LSN(f.lsn), pg.LSN(f.lsn)
		if f.streaming {
			s.Receiver = "streaming"
		}
	} else {
		s.CurrentLSN = pg.LSN(f.lsn)
	}
	return s, nil
}
func (f *fakeNode) Promote(context.Context, time.Duration) (bool, error) {
	defer f.lock()()
	if !f.promoteOK {
		return false, nil
	}
	f.inRecovery, f.streaming = false, false
	f.timeline++
	f.signals[pg.StandbySignal] = false
	return true, nil
}
func (f *fakeNode) Slots(context.Context) ([]pg.Slot, error) {
	defer f.lock()()
	var out []pg.Slot
	for n := range f.slots {
		out = append(out, pg.Slot{Name: n, Lost: f.lostSlots[n]})
	}
	return out, nil
}
func (f *fakeNode) CreateSlot(_ context.Context, n string) error {
	defer f.lock()()
	f.slots[n] = true
	return nil
}
func (f *fakeNode) DropSlot(_ context.Context, n string) error {
	defer f.lock()()
	delete(f.slots, n)
	delete(f.lostSlots, n)
	f.dropped = append(f.dropped, n)
	return nil
}
func (f *fakeNode) Archiver(context.Context) (pg.Archiver, error) { return pg.Archiver{}, nil }
func (f *fakeNode) Replicas(context.Context) ([]pg.Replica, error) {
	defer f.lock()()
	return f.reps, nil
}
func (f *fakeNode) PendingRestart(context.Context) ([]string, error) { return nil, nil }
func (f *fakeNode) ApplyRoles(context.Context, pg.Spec, pg.Credentials) error {
	defer f.lock()()
	f.roles++
	return nil
}
func (f *fakeNode) CloseConns() {}
func (f *fakeNode) Initdb(context.Context) error {
	defer f.lock()()
	f.hasData = true
	return nil
}
func (f *fakeNode) Clone(context.Context, string) error {
	defer f.lock()()
	f.hasData, f.sysid = true, f.cloneSysid
	f.clones++
	return nil
}
func (f *fakeNode) FetchBackup(context.Context, string, string) error {
	defer f.lock()()
	f.hasData = true
	return nil
}
func (f *fakeNode) Rewind(context.Context, string) (bool, error) {
	defer f.lock()()
	f.rewinds++
	return !f.rewindNoop, f.rewindErr
}
func (f *fakeNode) MoveAside(string) (string, error) {
	defer f.lock()()
	f.hasData = false
	f.asides++
	return "/data.aside", nil
}
func (f *fakeNode) Disk() (uint64, uint64, uint64, error) { return 100, 50, 1, nil }

// fakePeers answers status requests from a table.
type fakePeers struct {
	mu  sync.Mutex
	res map[string]peer.Result
}

func (p *fakePeers) set(pod string, r peer.Result) { p.mu.Lock(); p.res[pod] = r; p.mu.Unlock() }

func (p *fakePeers) FetchAll(_ context.Context, pods []string) map[string]peer.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]peer.Result{}
	for _, pod := range pods {
		if r, ok := p.res[pod]; ok {
			out[pod] = r
		} else {
			out[pod] = peer.Result{Err: errors.New("unreachable")}
		}
	}
	return out
}

var down = peer.Result{Err: errors.New("unreachable")}

func streamingStandby(pos uint64) peer.Result {
	return peer.Result{Status: peer.Status{Role: peer.RoleStandby, Running: true, HasData: true, InRecovery: true, Streaming: true, Eligible: true, Timeline: 1, Position: pos, SystemID: "100"}}
}

func idleStandby(pos uint64) peer.Result {
	r := streamingStandby(pos)
	r.Status.Streaming = false
	return r
}

func primaryPeer() peer.Result {
	return peer.Result{Status: peer.Status{Role: peer.RolePrimary, Running: true, HasData: true, Timeline: 1, SystemID: "100"}}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type harness struct {
	t     *testing.T
	a     *Agent
	node  *fakeNode
	peers *fakePeers
	cs    *fake.Clientset
	clock *clock
	m     *metrics.Set
}

var topo = cluster.Topology{Cluster: "pg", Namespace: "db", Headless: "pg-headless", Replicas: 3}

func newHarness(t *testing.T, me string, holder string, ann map[string]string) *harness {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "db", Annotations: ann}}
	if holder != "" {
		l.Spec.HolderIdentity = &holder
		rt := metav1.NewMicroTime(c.now())
		l.Spec.RenewTime = &rt
	}
	objs := []runtime.Object{l}
	for _, m := range topo.Members() {
		objs = append(objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: m, Namespace: "db"}})
	}
	cs := fake.NewClientset(objs...)
	store := lease.New(cs, "db", "pg", 15*time.Second)
	store.SetClock(c.now)
	cfg := config.Config{
		Topology: topo, PodName: me, Lease: "pg",
		DataDir: "/data", ConfigDir: "/cfg", SecretsDir: "/sec", TLSDir: "/tls", RunDir: "/run", BinDir: "/bin",
		LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		RejoinTimeout: 60 * time.Second, PromoteTimeout: 30 * time.Second, PeerTimeout: 2 * time.Second, ShutdownTimeout: 5 * time.Second,
		MaxLagOnFailover: 1 << 20, ReadyMaxLag: 1 << 20,
		Bootstrap: config.Bootstrap{Mode: config.BootstrapInitdb},
		SyncMode:  config.SyncQuorum, SyncCount: 1,
	}
	h := &harness{t: t, node: newNode(), peers: &fakePeers{res: map[string]peer.Result{}}, cs: cs, clock: c, m: metrics.New()}
	h.a = New(Options{
		Config: cfg, Log: golog.Nop(), Node: h.node, Leases: store, Peers: h.peers, Kube: kube.New(cs, "db"), Metrics: h.m,
		ReadSecret: func(string) (string, error) { return "pw", nil },
		LoadSpec:   func(string) (pg.Spec, error) { return pg.Spec{}, nil },
		FileStamp:  func(string) string { return "1" },
		Now:        c.now,
	})
	return h
}

func (h *harness) tick() {
	h.t.Helper()
	h.a.Tick(context.Background())
}

func (h *harness) settle() {
	h.t.Helper()
	if err := h.a.WaitTask(); err != nil {
		h.t.Logf("task error: %v", err)
	}
	h.tick()
}

func (h *harness) lease() lease.Record {
	h.t.Helper()
	l, err := h.cs.CoordinationV1().Leases("db").Get(context.Background(), "pg", metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	r := lease.Record{Annotations: l.Annotations}
	if l.Spec.HolderIdentity != nil {
		r.Holder = *l.Spec.HolderIdentity
	}
	return r
}

// holderRenews moves the clock on while the current holder keeps renewing.
func (h *harness) holderRenews(d time.Duration) {
	h.t.Helper()
	for step := 5 * time.Second; d > 0; d -= step {
		h.clock.add(min(step, d))
		l, err := h.cs.CoordinationV1().Leases("db").Get(context.Background(), "pg", metav1.GetOptions{})
		if err != nil {
			h.t.Fatal(err)
		}
		now := metav1.NewMicroTime(h.clock.now())
		l.Spec.RenewTime = &now
		if _, err := h.cs.CoordinationV1().Leases("db").Update(context.Background(), l, metav1.UpdateOptions{}); err != nil {
			h.t.Fatal(err)
		}
		h.tick()
	}
}

func (h *harness) renewTime() time.Time {
	h.t.Helper()
	l, err := h.cs.CoordinationV1().Leases("db").Get(context.Background(), "pg", metav1.GetOptions{})
	if err != nil || l.Spec.RenewTime == nil {
		return time.Time{}
	}
	return l.Spec.RenewTime.Time
}

func (h *harness) roleLabel(pod string) string {
	p, _ := h.cs.CoreV1().Pods("db").Get(context.Background(), pod, metav1.GetOptions{})
	return p.Labels[kube.RoleLabel]
}

func (h *harness) metric(name string) float64 {
	g, err := h.m.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	return g[name]
}

// --- bootstrap ---

func TestBootstrapLowestReachableMemberRunsInitdb(t *testing.T) {
	h := newHarness(t, "pg-0", "", nil)
	empty := peer.Result{Status: peer.Status{Role: peer.RoleStopped}}
	h.peers.set("pg-1", empty)
	h.peers.set("pg-2", empty)
	h.tick()
	if h.lease().Holder != "pg-0" {
		t.Fatal("bootstrap member did not take the lease")
	}
	h.settle() // initdb finishes, server starts read-write
	if !h.node.running || h.node.inRecovery {
		t.Fatalf("not running read-write after bootstrap: running=%v recovery=%v", h.node.running, h.node.inRecovery)
	}
	h.tick() // primary duties
	if h.roleLabel("pg-0") != kube.RolePrimary {
		t.Fatal("pod not labelled primary")
	}
	if !h.node.slots["pg_1"] || !h.node.slots["pg_2"] || h.node.slots["pg_0"] {
		t.Fatalf("slots = %v", h.node.slots)
	}
	h.a.WaitBackground()
	if h.node.roles != 1 {
		t.Fatalf("roles applied %d times", h.node.roles)
	}
	if h.lease().Ann(lease.SystemID) != "100" {
		t.Fatalf("system identifier not recorded: %v", h.lease().Annotations)
	}
	if !h.a.Ready() {
		t.Fatal("primary not ready")
	}
}

func TestBootstrapWaitsForLowerMemberAndNeverAssumesOrdinalZero(t *testing.T) {
	h := newHarness(t, "pg-1", "", nil)
	empty := peer.Result{Status: peer.Status{Role: peer.RoleStopped}}
	h.peers.set("pg-0", empty)
	h.peers.set("pg-2", empty)
	h.tick()
	if h.lease().Holder != "" || h.node.hasData {
		t.Fatal("pg-1 bootstrapped while pg-0 was available")
	}
	// pg-0 has gone: pg-1 is now the lowest reachable member.
	h.peers.set("pg-0", down)
	h.tick()
	if h.lease().Holder != "pg-1" {
		t.Fatal("pg-1 did not bootstrap with pg-0 gone")
	}
}

func TestEmptyMemberNeverInitdbsAnExistingCluster(t *testing.T) {
	h := newHarness(t, "pg-0", "", map[string]string{lease.SystemID: "100"})
	h.peers.set("pg-1", peer.Result{Status: peer.Status{Role: peer.RoleStopped}})
	h.peers.set("pg-2", peer.Result{Status: peer.Status{Role: peer.RoleStopped}})
	h.tick()
	if h.lease().Holder != "" || h.node.hasData {
		t.Fatal("bootstrapped over a cluster that already has a system identifier")
	}
}

func TestEmptyMemberClonesFromTheHolder(t *testing.T) {
	h := newHarness(t, "pg-0", "pg-1", map[string]string{lease.SystemID: "100"})
	h.peers.set("pg-1", primaryPeer())
	h.tick()
	h.settle()
	if h.node.clones != 1 || !h.node.signals[pg.StandbySignal] {
		t.Fatalf("clones=%d standby=%v", h.node.clones, h.node.signals[pg.StandbySignal])
	}
	if !h.node.running || !h.node.inRecovery {
		t.Fatal("clone not started as a standby")
	}
	if !strings.Contains(h.node.conf["primary_conninfo"], "host=pg-1.pg-headless.db.svc") {
		t.Fatalf("conninfo = %q", h.node.conf["primary_conninfo"])
	}
}

func TestReleasesLeaseWhenDataVanishes(t *testing.T) {
	h := newHarness(t, "pg-0", "pg-0", map[string]string{lease.SystemID: "100"})
	h.tick()
	if h.lease().Holder != "" {
		t.Fatal("kept the lease of an existing cluster with no data")
	}
}

// --- following, fencing, rejoin ---

func standbyNode(h *harness) {
	h.node.hasData, h.node.signals[pg.StandbySignal] = true, true
}

func TestStandbyFollowsHolderWithFullConninfo(t *testing.T) {
	h := newHarness(t, "pg-1", "pg-0", map[string]string{lease.SystemID: "100"})
	standbyNode(h)
	h.tick()
	c := h.node.conf["primary_conninfo"]
	for _, want := range []string{"host=pg-0.pg-headless.db.svc", "sslmode=verify-full", "application_name=pg-1", "connect_timeout=5", "keepalives=1"} {
		if !strings.Contains(c, want) {
			t.Errorf("conninfo %q lacks %q", c, want)
		}
	}
	if h.node.conf["primary_slot_name"] != "pg_1" {
		t.Fatalf("slot = %q", h.node.conf["primary_slot_name"])
	}
}

func TestStandbyRepointsWhenTheHolderChanges(t *testing.T) {
	h := newHarness(t, "pg-2", "pg-0", map[string]string{lease.SystemID: "100"})
	standbyNode(h)
	h.tick()
	h.node.streaming = true
	h.tick()
	// pg-1 takes over.
	l, _ := h.cs.CoordinationV1().Leases("db").Get(context.Background(), "pg", metav1.GetOptions{})
	p1 := "pg-1"
	l.Spec.HolderIdentity = &p1
	if _, err := h.cs.CoordinationV1().Leases("db").Update(context.Background(), l, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	reloads := h.node.reloads
	h.tick()
	if !strings.Contains(h.node.conf["primary_conninfo"], "host=pg-1.") || h.node.reloads != reloads+1 {
		t.Fatalf("not repointed: conninfo=%q reloads=%d", h.node.conf["primary_conninfo"], h.node.reloads)
	}
	if h.node.starts != 1 {
		t.Fatal("restarted instead of reloading")
	}
}

func TestFencesReadWriteServerWithoutTheLease(t *testing.T) {
	h := newHarness(t, "pg-0", "pg-1", map[string]string{lease.SystemID: "100"})
	h.node.hasData = true
	_ = h.node.Start() // comes back read-write on its old volume
	h.tick()
	if h.node.running || len(h.node.stops) != 1 || h.node.stops[0] != pg.StopImmediate {
		t.Fatalf("not fenced: running=%v stops=%v", h.node.running, h.node.stops)
	}
	if !h.node.signals[RejoinMarker] {
		t.Fatal("rejoin marker not set")
	}
	if h.metric("pgha_fencings_total") != 1 {
		t.Fatal("fencing not counted")
	}
	// Next round: rewind against the holder, then follow it.
	h.peers.set("pg-1", primaryPeer())
	h.tick()
	h.settle()
	if h.node.rewinds != 1 || h.node.signals[RejoinMarker] || !h.node.signals[pg.StandbySignal] {
		t.Fatalf("rejoin incomplete: rewinds=%d marker=%v standby=%v", h.node.rewinds, h.node.signals[RejoinMarker], h.node.signals[pg.StandbySignal])
	}
	if !h.node.running || !h.node.inRecovery {
		t.Fatal("former primary did not come back as a standby")
	}
}

func TestFailedRewindMovesDataAsideAndClones(t *testing.T) {
	h := newHarness(t, "pg-0", "pg-1", map[string]string{lease.SystemID: "100"})
	h.node.hasData = true
	h.node.signals[RejoinMarker] = true
	h.node.rewindErr = errors.New("timelines diverged too far")
	h.peers.set("pg-1", primaryPeer())
	h.tick()
	_ = h.a.WaitTask()
	if h.node.asides != 1 || h.node.clones != 1 {
		t.Fatalf("asides=%d clones=%d", h.node.asides, h.node.clones)
	}
}

func TestWatchdogFencesWhenRenewalsStop(t *testing.T) {
	h := newHarness(t, "pg-0", "", nil)
	h.node.hasData = true
	h.a.holdingRW.Store(true)
	_ = h.node.Start()
	h.a.lastRenew.Store(h.clock.now().UnixNano())
	h.clock.add(9 * time.Second)
	h.a.checkFence()
	if !h.node.running {
		t.Fatal("fenced before the deadline")
	}
	h.clock.add(2 * time.Second)
	h.a.checkFence()
	if h.node.running || h.node.stops[0] != pg.StopImmediate {
		t.Fatal("not fenced after the renew deadline")
	}
}

func TestRefusesForeignSystemIdentifier(t *testing.T) {
	h := newHarness(t, "pg-1", "pg-0", map[string]string{lease.SystemID: "999"})
	standbyNode(h)
	h.tick()
	if h.node.running {
		t.Fatal("started a data directory from another cluster")
	}
	if h.metric("pgha_errors_total,code=1102") != 1 {
		t.Fatal("mismatch not reported with its code")
	}
}

// --- failover ---

func expiredHarness(t *testing.T, me string) *harness {
	h := newHarness(t, me, "pg-0", map[string]string{lease.SystemID: "100", lease.Timeline: "1", lease.LSN: "200"})
	standbyNode(h)
	h.node.lsn = 200
	h.tick() // starts following pg-0
	h.clock.add(16 * time.Second)
	return h
}

func TestFailoverPromotesMostCompleteStandby(t *testing.T) {
	h := expiredHarness(t, "pg-1")
	// The dead primary's pod still carries its label.
	if err := kube.New(h.cs, "db").SetRole(context.Background(), "pg-0", kube.RolePrimary); err != nil {
		t.Fatal(err)
	}
	h.peers.set("pg-0", down)
	h.peers.set("pg-2", idleStandby(100))
	h.tick()
	if h.lease().Holder != "pg-1" {
		t.Fatalf("holder = %q", h.lease().Holder)
	}
	h.tick() // starts the promotion
	h.settle()
	if h.node.inRecovery {
		t.Fatal("not promoted")
	}
	h.tick()
	if h.roleLabel("pg-1") != kube.RolePrimary || h.roleLabel("pg-0") != kube.RoleNone {
		t.Fatalf("labels: pg-1=%q pg-0=%q", h.roleLabel("pg-1"), h.roleLabel("pg-0"))
	}
	if h.metric("pgha_promotions_total") != 1 {
		t.Fatal("promotion not counted")
	}
}

func TestNoFailoverWithoutQuorum(t *testing.T) {
	h := expiredHarness(t, "pg-1")
	h.peers.set("pg-0", down)
	h.peers.set("pg-2", down)
	h.tick()
	if h.lease().Holder != "pg-0" {
		t.Fatal("took the lease without a quorum")
	}
}

func TestNoFailoverWhileAPeerStillStreams(t *testing.T) {
	h := expiredHarness(t, "pg-1")
	h.peers.set("pg-0", down)
	h.peers.set("pg-2", streamingStandby(100))
	h.tick()
	if h.lease().Holder != "pg-0" {
		t.Fatal("took the lease while another member still sees the primary")
	}
}

func TestLessCompleteStandbyWaits(t *testing.T) {
	h := expiredHarness(t, "pg-1")
	h.peers.set("pg-0", down)
	h.peers.set("pg-2", idleStandby(300))
	h.tick()
	if h.lease().Holder != "pg-0" {
		t.Fatal("less complete standby took the lease")
	}
}

func TestFailedPromotionReleasesLeaseAndNeverRepointsService(t *testing.T) {
	h := expiredHarness(t, "pg-1")
	h.peers.set("pg-0", down)
	h.peers.set("pg-2", idleStandby(100))
	h.node.promoteOK = false
	h.tick()
	h.tick()
	_ = h.a.WaitTask()
	if h.lease().Holder != "" {
		t.Fatalf("lease kept after a failed promotion: %q", h.lease().Holder)
	}
	if h.roleLabel("pg-1") == kube.RolePrimary {
		t.Fatal("labelled primary without ever promoting")
	}
	if h.metric("pgha_promote_failures_total") != 1 || h.metric("pgha_promotions_total") != 0 {
		t.Fatal("promotion counters wrong")
	}
	if h.a.holdingRW.Load() {
		t.Fatal("still marked read-write")
	}
}

// --- switchover and shutdown ---

func primaryHarness(t *testing.T) *harness {
	h := newHarness(t, "pg-0", "pg-0", map[string]string{lease.SystemID: "100"})
	h.node.hasData = true
	h.tick() // renews, starts read-write
	h.tick() // primary duties
	if h.node.inRecovery || !h.node.running {
		t.Fatal("not a primary")
	}
	return h
}

func TestSwitchoverHandsOverToStreamingStandby(t *testing.T) {
	h := primaryHarness(t)
	h.peers.set("pg-1", streamingStandby(100))
	h.peers.set("pg-2", idleStandby(100))
	if err := lease.New(h.cs, "db", "pg", 15*time.Second).Annotate(context.Background(), map[string]string{lease.Switchover: "any"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	r := h.lease()
	if r.Holder != "" || r.Ann(lease.Successor) != "pg-1" || r.Ann(lease.Switchover) != "" {
		t.Fatalf("after switchover: holder=%q ann=%v", r.Holder, r.Annotations)
	}
	if h.node.running || h.node.stops[len(h.node.stops)-1] != pg.StopFast {
		t.Fatal("primary not stopped cleanly before releasing")
	}
}

func TestShutdownAsPrimaryReleasesToSuccessor(t *testing.T) {
	h := primaryHarness(t)
	h.peers.set("pg-2", streamingStandby(100))
	h.a.Shutdown(context.Background())
	if r := h.lease(); r.Holder != "" || r.Ann(lease.Successor) != "pg-2" {
		t.Fatalf("after shutdown: %+v", r)
	}
}

func TestPrimaryRestartsReadWriteWhileStillHolding(t *testing.T) {
	h := newHarness(t, "pg-0", "pg-0", map[string]string{lease.SystemID: "100"})
	h.node.hasData = true
	h.tick()
	if !h.node.running || h.node.inRecovery || h.node.timeline != 1 {
		t.Fatal("holder did not resume as primary on the same timeline")
	}
}

// --- readiness ---

func TestStandbyReadinessFollowsLag(t *testing.T) {
	h := newHarness(t, "pg-1", "pg-0", map[string]string{lease.SystemID: "100", lease.Timeline: "1", lease.LSN: "1000"})
	standbyNode(h)
	h.tick()
	h.node.streaming, h.node.lsn = true, 1000
	h.tick()
	if !h.a.Ready() {
		t.Fatal("caught-up standby not ready")
	}
	h.node.lsn = 0
	l, _ := h.cs.CoordinationV1().Leases("db").Get(context.Background(), "pg", metav1.GetOptions{})
	l.Annotations[lease.LSN] = "999999999"
	_, _ = h.cs.CoordinationV1().Leases("db").Update(context.Background(), l, metav1.UpdateOptions{})
	h.tick()
	if h.a.Ready() {
		t.Fatal("lagging standby still ready")
	}
}

func TestLiveReportsStalledLoop(t *testing.T) {
	h := newHarness(t, "pg-1", "pg-0", nil)
	h.tick()
	if err := h.a.Live(); err != nil {
		t.Fatal(err)
	}
	h.clock.add(time.Minute)
	if h.a.Live() == nil {
		t.Fatal("stalled loop reported live")
	}
}

// --- synchronous replication ---

func TestSyncNamesFollowStreamingStandbys(t *testing.T) {
	h := primaryHarness(t)
	if got := h.node.conf["synchronous_standby_names"]; got != "" {
		t.Fatalf("no standby attached, but sync names = %q", got)
	}
	h.node.reps = []pg.Replica{{Name: "pg-2", State: "streaming"}, {Name: "pg-1", State: "catchup"}, {Name: "outsider", State: "streaming"}}
	h.tick()
	if got := h.node.conf["synchronous_standby_names"]; got != `ANY 1 ("pg-2")` {
		t.Fatalf("sync names = %q", got)
	}
	h.node.reps = append(h.node.reps, pg.Replica{Name: "pg-1", State: "streaming"})
	h.tick()
	if got := h.node.conf["synchronous_standby_names"]; got != `ANY 1 ("pg-1", "pg-2")` {
		t.Fatalf("sync names = %q", got)
	}
}

func TestStrictSyncListsEveryOtherMember(t *testing.T) {
	h := newHarness(t, "pg-0", "pg-0", map[string]string{lease.SystemID: "100"})
	h.a.cfg.SyncMode, h.a.cfg.SyncCount = config.SyncStrict, 2
	h.node.hasData = true
	h.tick()
	h.tick()
	if got := h.node.conf["synchronous_standby_names"]; got != `ANY 2 ("pg-1", "pg-2")` {
		t.Fatalf("strict sync names = %q", got)
	}
}

// --- poolers ---

type fakePoolers struct {
	mu     sync.Mutex
	resets int
}

func (f *fakePoolers) Reset(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	return 2, nil
}

func TestNewPrimaryResetsThePoolersOncePerTerm(t *testing.T) {
	h := expiredHarness(t, "pg-1")
	fp := &fakePoolers{}
	h.a.poolers = fp
	h.peers.set("pg-0", down)
	h.peers.set("pg-2", idleStandby(100))
	h.tick()   // takes the lease
	h.tick()   // promotes
	h.settle() // primary duties
	h.tick()
	h.a.WaitBackground()
	if fp.resets != 1 {
		t.Fatalf("poolers reset %d times, want 1", fp.resets)
	}
}

func TestShutdownKeepsRenewingThroughASlowStop(t *testing.T) {
	h := primaryHarness(t)
	h.a.cfg.RetryPeriod = 100 * time.Millisecond
	h.peers.set("pg-2", streamingStandby(100))
	before := h.renewTime()
	var during time.Time
	h.node.onStop = func() {
		// The fast shutdown takes longer than the lease duration.
		h.clock.add(20 * time.Second)
		time.Sleep(400 * time.Millisecond)
		during = h.renewTime()
	}
	h.a.Shutdown(context.Background())
	if !during.After(before) {
		t.Fatalf("the lease was not renewed during the stop (before %s, during %s)", before, during)
	}
	if r := h.lease(); r.Holder != "" || r.Ann(lease.Successor) != "pg-2" {
		t.Fatalf("after shutdown: %+v", r)
	}
}

func TestShutdownFencesWhenRenewalsFailDuringTheStop(t *testing.T) {
	h := primaryHarness(t)
	h.a.cfg.RetryPeriod = 100 * time.Millisecond
	h.peers.set("pg-2", streamingStandby(100))
	stops := 0
	h.node.onStop = func() {
		stops++
		if stops > 1 {
			return
		}
		// The API goes away and the stop hangs past the renew deadline.
		h.cs.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api server unreachable")
		})
		h.clock.add(11 * time.Second)
		time.Sleep(400 * time.Millisecond)
	}
	h.a.Shutdown(context.Background())
	immediate := false
	for _, m := range h.node.stops {
		if m == pg.StopImmediate {
			immediate = true
		}
	}
	if !immediate {
		t.Fatalf("not fenced while the stop hung without renewals: stops=%v", h.node.stops)
	}
}

// --- standbys that fell behind the retained WAL ---

func TestPrimaryRecreatesALostSlot(t *testing.T) {
	h := primaryHarness(t)
	h.node.lostSlots = map[string]bool{"pg_1": true}
	h.node.slots["pg_1"] = true
	h.clock.add(dutyEvery)
	h.tick()
	if len(h.node.dropped) != 1 || h.node.dropped[0] != "pg_1" || !h.node.slots["pg_1"] {
		t.Fatalf("lost slot not recreated: dropped=%v slots=%v", h.node.dropped, h.node.slots)
	}
	h.clock.add(dutyEvery)
	h.tick()
	if len(h.node.dropped) != 1 {
		t.Fatalf("a healthy slot was dropped: %v", h.node.dropped)
	}
}

func TestStuckStandbyRecloneAfterANoopRewind(t *testing.T) {
	h := newHarness(t, "pg-1", "pg-0", map[string]string{lease.SystemID: "100"})
	standbyNode(h)
	h.node.rewindNoop = true
	h.peers.set("pg-0", primaryPeer())
	stuck := func() {
		h.tick()                                            // running, not streaming
		h.holderRenews(h.a.cfg.RejoinTimeout + time.Second) // stuck: stops for a rejoin
		h.tick()                                            // starts the rejoin
		if err := h.a.WaitTask(); err != nil {
			t.Fatal(err)
		}
		h.tick() // starts again as a standby
	}
	stuck()
	if h.node.rewinds != 1 || h.node.clones != 0 {
		t.Fatalf("first rejoin: rewinds=%d clones=%d", h.node.rewinds, h.node.clones)
	}
	stuck()
	if h.node.clones != 1 || h.node.asides != 1 {
		t.Fatalf("a standby still stuck after a no-op rewind was not re-cloned: rewinds=%d clones=%d asides=%d", h.node.rewinds, h.node.clones, h.node.asides)
	}
}
