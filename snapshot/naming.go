// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of castai-guardrails-controllers

package snapshot

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	// MaxCRDNameLen is the k8s object-name length limit (DNS-1123 label).
	MaxCRDNameLen = 63
	// uidHashBytes is the number of sha256(uid) bytes included in the name.
	// 4 bytes -> 8 hex chars when formatted with %x.
	uidHashBytes = 4
)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// CollisionSafeName returns a DNS-1123-safe CRD name derived from workload
// identity. Format: <namespace>-<kind>-<name>-<uidhash8> where:
//   - namespace is truncated from the start if too long (tail is preserved
//     so the workload name portion remains identifiable)
//   - kind is lowercased
//   - name is truncated so the total length is ≤ MaxCRDNameLen
//   - uidhash8 is the first 8 hex chars of sha256(uid), guaranteeing uniqueness
//     across workloads sharing the same namespace/name after recreate.
func CollisionSafeName(kind, namespace, name string, uid types.UID) string {
	kindLower := strings.ToLower(kind)
	nsLower := strings.ToLower(namespace)
	uidHexLen := 2 * uidHashBytes

	// If namespace is empty, fall back to the legacy format to keep DNS-1123
	// validity (no leading dash).
	if nsLower == "" {
		maxNameLen := MaxCRDNameLen - len(kindLower) - 2 - uidHexLen
		if maxNameLen < 1 {
			maxNameLen = 1
		}
		if len(name) > maxNameLen {
			name = name[:maxNameLen]
		}
		sum := sha256.Sum256([]byte(string(uid)))
		return fmt.Sprintf("%s-%s-%x", kindLower, name, sum[:uidHashBytes])
	}

	// total = len(nsLower) + 1 + len(kindLower) + 1 + len(name) + 1 + uidHexLen
	maxNameLen := MaxCRDNameLen - len(nsLower) - 1 - len(kindLower) - 2 - uidHexLen
	if maxNameLen < 1 {
		maxNameLen = 1
	}
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	sum := sha256.Sum256([]byte(string(uid)))
	return fmt.Sprintf("%s-%s-%s-%x", nsLower, kindLower, name, sum[:uidHashBytes])
}

// IsDNS1123Label reports whether s matches the DNS-1123 label regex.
func IsDNS1123Label(s string) bool {
	return dns1123Label.MatchString(s)
}
