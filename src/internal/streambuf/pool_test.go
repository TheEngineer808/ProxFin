package streambuf

import "testing"

func TestSizeClasses(t *testing.T) {
	prev := 0
	for _, size := range classSizes {
		if size <= prev || (prev > 0 && size > prev*3/2+1) {
			t.Fatalf("classes %v: steps must grow by at most 1.5x", classSizes)
		}
		prev = size
	}
	if classSizes[len(classSizes)-1] < 64<<10+2*tsPacketSize {
		t.Fatalf("largest class %d is below the largest chunk", prev)
	}
	for _, n := range []int{1, 2048, 2049, 40000, 65536 + 376} {
		c := classOf(n)
		if c < 0 || classSizes[c] < n || (c > 0 && classSizes[c-1] >= n) {
			t.Fatalf("classOf(%d) = %d", n, c)
		}
	}
	if classOf(1<<20) != -1 {
		t.Fatalf("oversized data must not be pooled")
	}
}
