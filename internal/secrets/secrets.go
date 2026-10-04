// Package secrets creates the cluster's generated credentials once. The chart
// runs it as a pre-install and pre-upgrade hook Job, so a Secret is created
// the first time it is needed and never rewritten, whether the chart is
// installed by Helm or rendered by a GitOps tool without cluster access.
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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	golog "github.com/Bugs5382/go-log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Item is one Secret to ensure.
type Item struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	// Labels are added to a Secret this package creates.
	Labels map[string]string `json:"labels,omitempty"`
}

// Spec is the list the chart renders.
type Spec struct {
	Items []Item `json:"items"`
}

// LoadSpec reads the spec file.
func LoadSpec(path string) (Spec, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- chart-mounted spec path
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Password returns a random password of n characters. Letters and digits
// only, so it is safe in connection strings, password files and URIs.
func Password(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b), nil
}

// Ensure creates every missing Secret and leaves existing ones alone. It
// returns how many it created.
func Ensure(ctx context.Context, cs kubernetes.Interface, ns string, spec Spec, log golog.Logger) (int, error) {
	created := 0
	for _, it := range spec.Items {
		_, err := cs.CoreV1().Secrets(ns).Get(ctx, it.Name, metav1.GetOptions{})
		if err == nil {
			log.Info("secret exists; leaving it as it is", golog.F("secret", it.Name))
			continue
		}
		if !apierrors.IsNotFound(err) {
			return created, fmt.Errorf("get secret %s: %w", it.Name, err)
		}
		pw, err := Password(32)
		if err != nil {
			return created, err
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: it.Name, Namespace: ns, Labels: it.Labels},
			Type:       corev1.SecretTypeOpaque,
			StringData: map[string]string{"username": it.Username, "password": pw},
		}
		if _, err := cs.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				log.Info("secret created concurrently; leaving it", golog.F("secret", it.Name))
				continue
			}
			return created, fmt.Errorf("create secret %s: %w", it.Name, err)
		}
		created++
		log.Info("secret created", golog.F("secret", it.Name))
	}
	return created, nil
}
