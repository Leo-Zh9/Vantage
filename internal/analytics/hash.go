package analytics

// hash64 is an allocation-free FNV-1a over the string, run through a SplitMix64
// finalizer. FNV-1a alone has weak bit avalanche, which skews HyperLogLog
// registers and Count-Min Sketch buckets; the finalizer scrambles the output so
// the high and low halves are usable as two near-independent 32-bit hashes.
func hash64(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return splitmix64(h)
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
