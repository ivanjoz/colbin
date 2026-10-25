//go:build arm64 && !purego

package column

// NEON kernels for the encoder's scans, in assembly (asm_arm64.s), for every
// arm64 build that is not purego: the same scans asm_amd64.s does in AVX2,
// behind the same Go side, vec.go.

// useSIMD: Advanced SIMD is part of the arm64 baseline Go builds for, so
// every arm64 CPU runs the kernels and there is nothing to detect.
const useSIMD = true
