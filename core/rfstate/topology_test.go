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

import "testing"

func TestLayoutCPUsGroupsByCache(t *testing.T) {
	in := []int{10, 11, 2, 3}
	groups := map[int]string{10: "10-11", 11: "10-11", 2: "2-3", 3: "2-3"}
	out, labels, ok := layoutCPUs(in, func(cpu int) (string, bool) {
		lab, yes := groups[cpu]
		return lab, yes
	})
	if !ok {
		t.Fatal("expected groups")
	}
	want := []int{2, 3, 10, 11}
	if len(out) != len(want) {
		t.Fatalf("len %d", len(out))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("order %v", out)
		}
	}
	if len(labels) != 2 || labels[0] != "2-3" || labels[1] != "10-11" {
		t.Fatalf("labels %v", labels)
	}
}

func TestLayoutCPUsKeepsOrderWithoutSysfs(t *testing.T) {
	in := []int{5, 1, 9}
	out, labels, ok := layoutCPUs(in, func(int) (string, bool) { return "", false })
	if ok {
		t.Fatal("expected missing sysfs")
	}
	if labels != nil || len(out) != 3 || out[0] != 5 || out[1] != 1 || out[2] != 9 {
		t.Fatalf("out %v labels %v", out, labels)
	}
}

func TestLayoutCPUsFillsOneGroupBeforeSpill(t *testing.T) {
	// 129-136 straddles two 8-CPU caches. The input starts on the second.
	in := []int{136, 129, 130, 131, 132, 133, 134, 135}
	groupOf := func(cpu int) (string, bool) {
		if cpu >= 128 && cpu <= 135 {
			return "128-135", true
		}
		if cpu >= 136 && cpu <= 143 {
			return "136-143", true
		}
		return "", false
	}
	out, labels, ok := layoutCPUs(in, groupOf)
	if !ok {
		t.Fatal("expected groups")
	}
	want := []int{129, 130, 131, 132, 133, 134, 135, 136}
	if len(out) != len(want) {
		t.Fatalf("len %d", len(out))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("order %v", out)
		}
	}
	if len(labels) != 2 || labels[0] != "128-135" || labels[1] != "136-143" {
		t.Fatalf("labels %v", labels)
	}
}

func TestLayoutCPUsLiveSysfs(t *testing.T) {
	in := []int{0, 1, 2, 3}
	out, _ := LayoutCPUs(in)
	if len(out) != len(in) {
		t.Fatalf("len %d", len(out))
	}
	seen := map[int]bool{}
	for _, cpu := range out {
		if seen[cpu] {
			t.Fatalf("duplicate %v", out)
		}
		seen[cpu] = true
	}
	for _, cpu := range in {
		if !seen[cpu] {
			t.Fatalf("missing %d in %v", cpu, out)
		}
	}
}
