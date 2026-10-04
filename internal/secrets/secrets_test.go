package secrets

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
	"strings"
	"testing"

	golog "github.com/Bugs5382/go-log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureCreatesMissingAndKeepsExisting(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-superuser", Namespace: "db"},
		Data:       map[string][]byte{"password": []byte("keep-me")},
	}
	cs := fake.NewClientset(existing)
	spec := Spec{Items: []Item{
		{Name: "pg-superuser", Username: "postgres"},
		{Name: "pg-replication", Username: "replicator", Labels: map[string]string{"app": "pg"}},
	}}
	ctx := context.Background()
	n, err := Ensure(ctx, cs, "db", spec, golog.Nop())
	if err != nil || n != 1 {
		t.Fatalf("created %d, err %v", n, err)
	}
	su, _ := cs.CoreV1().Secrets("db").Get(ctx, "pg-superuser", metav1.GetOptions{})
	if string(su.Data["password"]) != "keep-me" {
		t.Fatal("existing secret was rewritten")
	}
	rep, _ := cs.CoreV1().Secrets("db").Get(ctx, "pg-replication", metav1.GetOptions{})
	if len(rep.StringData["password"]) != 32 || rep.StringData["username"] != "replicator" || rep.Labels["app"] != "pg" {
		t.Fatalf("created secret = %+v", rep)
	}
	// A second run (an upgrade, or a GitOps resync) changes nothing.
	n, err = Ensure(ctx, cs, "db", spec, golog.Nop())
	if err != nil || n != 0 {
		t.Fatalf("second run created %d, err %v", n, err)
	}
	rep2, _ := cs.CoreV1().Secrets("db").Get(ctx, "pg-replication", metav1.GetOptions{})
	if rep2.StringData["password"] != rep.StringData["password"] {
		t.Fatal("generated password changed on the second run")
	}
}

func TestPassword(t *testing.T) {
	a, err := Password(32)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Password(32)
	if a == b || len(a) != 32 || strings.Trim(a, alphabet) != "" {
		t.Fatalf("passwords %q %q", a, b)
	}
}
