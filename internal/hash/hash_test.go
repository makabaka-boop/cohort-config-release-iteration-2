package hash

import "testing"

func TestBucketStableAndInRange(t *testing.T) {
	b1 := Bucket("client-abc")
	b2 := Bucket("client-abc")
	if b1 != b2 {
		t.Fatalf("bucket not stable: %d vs %d", b1, b2)
	}
	if b1 >= BucketCount {
		t.Fatalf("bucket out of range: %d", b1)
	}
}

func TestBucketDistributes(t *testing.T) {
	// 粗略检查：1000 个不同客户端应落在多个不同桶上。
	seen := map[uint64]bool{}
	for i := 0; i < 1000; i++ {
		seen[Bucket("client-"+itoa(i))] = true
	}
	if len(seen) < 50 {
		t.Fatalf("distribution looks broken: only %d distinct buckets", len(seen))
	}
}

func TestInTrialBoundaries(t *testing.T) {
	if InTrial(0, 0) {
		t.Error("0% must include nobody")
	}
	if InTrial(0, 100) != true || InTrial(99, 100) != true {
		t.Error("100% must include all buckets")
	}
	if !InTrial(9, 10) || InTrial(10, 10) {
		t.Error("10% must include buckets 0..9 only")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
