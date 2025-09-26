package v1alpha1

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	u "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

//
//
//
// this code was heavily chatgpt'd back and forth to get it right
// heres the spec I wanted to support:

// # Example: Promise with an upgrade strategy expressed as ordered "waves".
// # Rules:
// # - Each wave selects from the REMAINING set after previous waves.
// # - Everything within a wave runs concurrently.
// # - Overlaps are allowed. Later waves only see what is still remaining.
// # - Selection primitives:
// #     names: ["ns/name", ...]
// #     matchLabels: {key: value, ...}           # resource labels
// #     namespaces: ["ns-a", "ns-b"]             # explicit namespace names
// #     namespaceSelector: { matchLabels: {...} }# namespace labels
// #     remainder: true                          # everything left
//
// apiVersion: marketplace.kratix.io/v1alpha1
// kind: Promise
// metadata:
//   name: redis
// spec:
//   upgradeStrategy:
//     waves:
//       # 1) Hand-picked canaries. Always upgrade these first.
//       - name: canaries
//         select:
//           names:
//             - dev-a/redis-canary
//             - prod/redis-canary
//
//       # 2) All dev resources that live in "gold" namespaces.
//       #    Here we combine resource labels with namespace labels.
//       - name: dev-in-gold-namespaces
//         select:
//           matchLabels: { env: dev }             # resource has env=dev
//           namespaceSelector:
//             matchLabels: { tier: gold }         # namespace has tier=gold
//
//       # 3) Everything left in two specific dev namespaces, regardless of resource labels.
//       #    This can pick up stragglers missed by label-based selection.
//       - name: remainder-in-dev-a-and-dev-b
//         select:
//           namespaces: ["dev-a", "dev-b"]
//
//       # 4) All staging resources in the EU region namespaces.
//       - name: staging-in-eu
//         select:
//           matchLabels: { env: staging }
//           namespaceSelector:
//             matchLabels:
//               region: europe
//
//       # 5) Anything left that is env=dev across the entire cluster.
//       #    Overlaps are fine. Previous waves already removed matched items.
//       - name: remainder-env-dev
//         select:
//           matchLabels: { env: dev }
//
//       # 6) All prod resources in gold namespaces.
//       - name: prod-in-gold
//         select:
//           matchLabels: { env: prod }
//           namespaceSelector:
//             matchLabels: { tier: gold }
//
//       # 7) All prod resources in explicitly named namespaces that are not gold.
//       - name: prod-in-named-namespaces
//         select:
//           matchLabels: { env: prod }
//           namespaces: ["prod-a", "prod-b", "prod-c"]
//
//       # 8) Final catch-all. Anything still not upgraded goes now.
//       - name: everything-else
//         select:
//           remainder: true

// UpgradeStrategy describes ordered waves. All items in a wave are processed concurrently.
type UpgradeStrategy struct {
	// Waves are evaluated in order against the remaining set. Overlaps are allowed.
	Waves []UpgradeWave `json:"waves,omitempty"`
}

type UpgradeWave struct {
	// Optional label for humans
	Name string `json:"name,omitempty"`

	// Required selector for this wave
	Select WaveSelect `json:"select"`
}

type WaveSelect struct {
	// Exactly one of: names, selector, or remainder

	Names []string `json:"names,omitempty"`

	// Resource label selector
	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	// Optional namespace filter as explicit names
	Namespaces []string `json:"namespaces,omitempty"`

	// Optional namespace selector by labels
	NamespaceSelector *NamespaceSelector `json:"namespaceSelector,omitempty"`

	// Select everything not matched by previous waves
	Remainder bool `json:"remainder,omitempty"`
}

type NamespaceSelector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// CompileUpgradeStrategy returns waves of "namespace/name".
// nsLabels is optional. If provided, it must map ns -> labels for namespaceSelector evaluation.
func (p *Promise) CompileUpgradeStrategy(rrList *u.UnstructuredList, nsLabels map[string]map[string]string) [][]string {
	idx := indexByKey(rrList) // key -> obj

	// default: single random wave of all keys
	if p == nil || p.Spec.UpgradeStrategy == nil || len(p.Spec.UpgradeStrategy.Waves) == 0 {
		keys := mapKeys(idx)
		shuffle(keys, seedFrom(p))
		return [][]string{keys}
	}

	remaining := copyMap(idx)
	out := make([][]string, 0, len(p.Spec.UpgradeStrategy.Waves))

	for _, w := range p.Spec.UpgradeStrategy.Waves {
		cands := selectWave(w.Select, remaining, nsLabels)
		// Stable by name inside a wave. No ordering knob.
		sort.Strings(cands)

		if len(cands) > 0 {
			out = append(out, cands)
			for _, k := range cands {
				delete(remaining, k)
			}
		}
	}

	return out
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func indexByKey(rrList *u.UnstructuredList) map[string]*u.Unstructured {
	m := make(map[string]*u.Unstructured, len(rrList.Items))
	for i := range rrList.Items {
		o := &rrList.Items[i]
		ns := o.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		k := fmt.Sprintf("%s/%s", ns, o.GetName())
		m[k] = o
	}
	return m
}

func copyMap(src map[string]*u.Unstructured) map[string]*u.Unstructured {
	dst := make(map[string]*u.Unstructured, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mapKeys(m map[string]*u.Unstructured) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func nsOf(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return ""
}

func selectWave(sel WaveSelect, remaining map[string]*u.Unstructured, nsLabels map[string]map[string]string) []string {
	// names takes precedence
	if len(sel.Names) > 0 {
		out := make([]string, 0, len(sel.Names))
		for _, k := range sel.Names {
			if _, ok := remaining[k]; ok {
				out = append(out, k)
			}
		}
		return out
	}

	// build namespace allow set if provided
	nsAllow := map[string]bool{}
	if len(sel.Namespaces) > 0 {
		for _, ns := range sel.Namespaces {
			nsAllow[ns] = true
		}
	}
	nsSel := sel.NamespaceSelector

	// remainder
	if sel.Remainder {
		out := make([]string, 0, len(remaining))
		for k := range remaining {
			ns := nsOf(k)
			if len(nsAllow) > 0 && !nsAllow[ns] {
				continue
			}
			if nsSel != nil && !namespaceMatches(ns, nsSel, nsLabels) {
				continue
			}
			out = append(out, k)
		}
		return out
	}

	// selector on resource labels and optional namespace filter
	out := []string{}
	for k, o := range remaining {
		ns := o.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		if len(nsAllow) > 0 && !nsAllow[ns] {
			continue
		}
		if nsSel != nil && !namespaceMatches(ns, nsSel, nsLabels) {
			continue
		}
		if matchLabels(o.GetLabels(), sel.MatchLabels) {
			out = append(out, k)
		}
	}
	return out
}

func matchLabels(have, need map[string]string) bool {
	if len(need) == 0 {
		return true
	}
	for k, v := range need {
		if have[k] != v {
			return false
		}
	}
	return true
}

// namespaceMatches uses nsLabels if provided. If nsLabels is nil, only Namespaces filter applies.
func namespaceMatches(ns string, sel *NamespaceSelector, nsLabels map[string]map[string]string) bool {
	if sel == nil {
		return true
	}
	if nsLabels == nil {
		// No label info. Treat selector as not satisfied unless it is empty.
		return len(sel.MatchLabels) == 0
	}
	labels := nsLabels[ns]
	return matchLabels(labels, sel.MatchLabels)
}

func shuffle(keys []string, seed int64) {
	r := rand.New(rand.NewSource(seed))
	for i := len(keys) - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		keys[i], keys[j] = keys[j], keys[i]
	}
}

func seedFrom(p *Promise) int64 {
	if p == nil || len(p.UID) == 0 {
		return time.Now().UnixNano()
	}
	var s int64
	for _, b := range []byte(p.UID) {
		s = s*131 + int64(b)
	}
	return s
}
