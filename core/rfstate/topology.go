// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rfstate

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// LayoutCPUs reorders a pin list so the first active workers share a
// last-level cache before any worker spills into the next one. The group
// key is the highest-index cache's shared_cpu_list under
// /sys/devices/system/cpu/cpuN/cache (the CCX on EPYC parts, whatever size
// the kernel reports — the size is not hardcoded). Groups are ordered by
// their smallest CPU id, and CPUs inside a group are sorted.
//
// Harness mapping: cmd/rfbench passes the reordered list to NewPool, which
// pins worker i to ordered[i%len]. The pool's active set is the lowest
// worker ids, so it is a prefix of this list. A pin list that straddles two
// caches (for example 129-136 on a machine whose caches are 128-135 and
// 136-143) is rewritten so the whole first cache comes before the spill.
// If sysfs is missing for any CPU, the input order is returned and groups
// is nil; the harness then pins worker i to the caller's i-th CPU.
func LayoutCPUs(cpus []int) (ordered []int, groups []string) {
	ordered, groups, _ = layoutCPUs(cpus, readCacheGroup)
	return ordered, groups
}

func layoutCPUs(cpus []int, groupOf func(cpu int) (string, bool)) ([]int, []string, bool) {
	if len(cpus) == 0 {
		return nil, nil, true
	}
	labels := make([]string, len(cpus))
	for i, cpu := range cpus {
		label, ok := groupOf(cpu)
		if !ok || label == "" {
			return append([]int(nil), cpus...), nil, false
		}
		labels[i] = label
	}
	order := make([]string, 0)
	members := make(map[string][]int)
	minCPU := make(map[string]int)
	seen := make(map[string]bool)
	for i, cpu := range cpus {
		label := labels[i]
		if !seen[label] {
			seen[label] = true
			order = append(order, label)
			minCPU[label] = cpu
		} else if cpu < minCPU[label] {
			minCPU[label] = cpu
		}
		members[label] = append(members[label], cpu)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return minCPU[order[i]] < minCPU[order[j]]
	})
	out := make([]int, 0, len(cpus))
	for _, label := range order {
		cs := members[label]
		sort.Ints(cs)
		out = append(out, cs...)
	}
	return out, order, true
}

// readCacheGroup returns the shared_cpu_list of cpu's highest-index cache.
func readCacheGroup(cpu int) (string, bool) {
	dir := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cache", cpu)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	best := -1
	bestName := ""
	for _, e := range entries {
		name := e.Name()
		rest, ok := strings.CutPrefix(name, "index")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < best {
			continue
		}
		best = n
		bestName = name
	}
	if bestName == "" {
		return "", false
	}
	raw, err := os.ReadFile(dir + "/" + bestName + "/shared_cpu_list")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}
