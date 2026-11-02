package postgres

import "testing"

func TestSameStringSetIgnoresOrderOnly(t *testing.T) {
	if !sameStringSet([]string{"pa_a", "pa_b"}, []string{"pa_b", "pa_a"}) {
		t.Fatal("sameStringSet rejected an order-only difference")
	}
	for _, pair := range [][2][]string{
		{{"pa_a"}, {"pa_b"}},
		{{"pa_a", "pa_a"}, {"pa_a"}},
		{{}, {"pa_a"}},
	} {
		if sameStringSet(pair[0], pair[1]) {
			t.Errorf("sameStringSet(%v,%v) = true", pair[0], pair[1])
		}
	}
}
