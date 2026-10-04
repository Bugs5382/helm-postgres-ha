// Package kube holds the one Kubernetes write a member makes besides the
// Lease: the role label the primary and replica Services select on. The
// chart owns the Services and never has them patched, so a Helm upgrade or a
// GitOps resync cannot point clients at the wrong member.
package kube

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
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Labels the agent reads and writes.
const (
	// RoleLabel is primary, replica or none. The primary Service selects
	// primary and the replica Service selects replica.
	RoleLabel = "postgres-ha/role"
)

// Role label values.
const (
	RolePrimary = "primary"
	RoleReplica = "replica"
	// RoleNone marks a member that is neither, such as one being rebuilt.
	RoleNone = "none"
)

// Client makes the writes in one namespace.
type Client struct {
	cs kubernetes.Interface
	ns string
}

// New returns a Client.
func New(cs kubernetes.Interface, ns string) *Client { return &Client{cs: cs, ns: ns} }

// Role returns pod's role label, or "" when it has none.
func (c *Client) Role(ctx context.Context, pod string) (string, error) {
	p, err := c.cs.CoreV1().Pods(c.ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get pod %s: %w", pod, err)
	}
	return p.Labels[RoleLabel], nil
}

// SetRole labels pod with role.
func (c *Client) SetRole(ctx context.Context, pod, role string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{RoleLabel: role}}})
	if err != nil {
		return err
	}
	if _, err := c.cs.CoreV1().Pods(c.ns).Patch(ctx, pod, types.StrategicMergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label pod %s %s=%s: %w", pod, RoleLabel, role, err)
	}
	return nil
}
