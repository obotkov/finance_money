package main

import "testing"

func TestCategoryTree(t *testing.T) {
	p := func(id int64) *int64 { return &id }
	have := map[int64]bool{1: true, 2: true, 3: true, 4: true, 5: true}
	cases := []struct {
		name  string
		items []CategoryPlace
		ok    bool
	}{
		{"flat", []CategoryPlace{{1, nil}, {2, nil}, {3, nil}, {4, nil}, {5, nil}}, true},
		{"four levels", []CategoryPlace{{1, nil}, {2, p(1)}, {3, p(2)}, {4, p(3)}, {5, nil}}, true},
		{"five levels", []CategoryPlace{{1, nil}, {2, p(1)}, {3, p(2)}, {4, p(3)}, {5, p(4)}}, false},
		{"cycle", []CategoryPlace{{1, p(2)}, {2, p(1)}, {3, nil}, {4, nil}, {5, nil}}, false},
		{"self", []CategoryPlace{{1, p(1)}, {2, nil}, {3, nil}, {4, nil}, {5, nil}}, false},
		{"missing", []CategoryPlace{{1, nil}, {2, nil}, {3, nil}, {4, nil}}, false},
		{"duplicate", []CategoryPlace{{1, nil}, {1, nil}, {3, nil}, {4, nil}, {5, nil}}, false},
		{"foreign", []CategoryPlace{{1, nil}, {2, nil}, {3, nil}, {4, nil}, {9, nil}}, false},
		{"foreign parent", []CategoryPlace{{1, p(9)}, {2, nil}, {3, nil}, {4, nil}, {5, nil}}, false},
	}
	for _, c := range cases {
		err := CategoryOrder{Kind: "expense", Items: c.items}.checkTree(have)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
}
