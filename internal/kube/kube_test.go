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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSetRole(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg-1", Namespace: "db", Labels: map[string]string{"app": "x"}}}
	cs := fake.NewClientset(pod)
	if err := New(cs, "db").SetRole(context.Background(), "pg-1", RoleReplica); err != nil {
		t.Fatal(err)
	}
	p, _ := cs.CoreV1().Pods("db").Get(context.Background(), "pg-1", metav1.GetOptions{})
	if p.Labels[RoleLabel] != RoleReplica || p.Labels["app"] != "x" {
		t.Fatalf("labels = %v", p.Labels)
	}
	if r, err := New(cs, "db").Role(context.Background(), "pg-1"); err != nil || r != RoleReplica {
		t.Fatalf("role = %q %v", r, err)
	}
}
