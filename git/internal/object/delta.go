package object

import (
	"errors"
	"fmt"
)

var errBadDelta = errors.New("malformed delta")

// readSize decodes one value in the pack "size encoding" (seven bits per
// byte, least significant first) and returns it with the bytes consumed;
// n is 0 when b ends early or the value overflows.
func readSize(b []byte) (v uint64, n int) {
	for i, c := range b {
		if i == 10 {
			return 0, 0
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// deltaSizes returns the base and result sizes a delta starts with, and the
// instructions after them.
func deltaSizes(delta []byte) (base, result uint64, ops []byte, err error) {
	base, n := readSize(delta)
	if n == 0 {
		return 0, 0, nil, errBadDelta
	}
	result, m := readSize(delta[n:])
	if m == 0 {
		return 0, 0, nil, errBadDelta
	}
	return base, result, delta[n+m:], nil
}

// applyDelta reconstructs an object from its base and a delta.
func applyDelta(base, delta []byte) ([]byte, error) {
	baseSize, resultSize, ops, err := deltaSizes(delta)
	if err != nil {
		return nil, err
	}
	if baseSize != uint64(len(base)) {
		return nil, fmt.Errorf("%w: base is %d bytes, delta expects %d", errBadDelta, len(base), baseSize)
	}
	// Validate every instruction and the size they produce before
	// allocating, so the declared size alone never decides an allocation:
	// a hostile delta could declare terabytes over a stream of bad opcodes.
	if err := walkDelta(base, ops, resultSize, nil); err != nil {
		return nil, err
	}
	// Even validated, a small delta can expand a large base many times over;
	// past maxInitialAlloc the buffer grows with the output instead.
	out := make([]byte, 0, min(resultSize, maxInitialAlloc))
	walkDelta(base, ops, resultSize, func(b []byte) { out = append(out, b...) })
	return out, nil
}

// walkDelta checks delta instructions ops against base and the declared
// result size, passing each produced span to emit when it is not nil.
func walkDelta(base, ops []byte, resultSize uint64, emit func([]byte)) error {
	var n uint64 // bytes produced so far
	for len(ops) > 0 {
		op := ops[0]
		ops = ops[1:]
		switch {
		case op&0x80 != 0:
			var off, size uint64
			for i := range 7 {
				if op&(1<<i) == 0 {
					continue
				}
				if len(ops) == 0 {
					return errBadDelta
				}
				if i < 4 {
					off |= uint64(ops[0]) << (8 * i)
				} else {
					size |= uint64(ops[0]) << (8 * (i - 4))
				}
				ops = ops[1:]
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > uint64(len(base)) || n+size > resultSize {
				return fmt.Errorf("%w: copy out of range", errBadDelta)
			}
			if emit != nil {
				emit(base[off : off+size])
			}
			n += size
		case op != 0:
			k := int(op)
			if k > len(ops) || n+uint64(k) > resultSize {
				return fmt.Errorf("%w: insert out of range", errBadDelta)
			}
			if emit != nil {
				emit(ops[:k])
			}
			n += uint64(k)
			ops = ops[k:]
		default:
			return fmt.Errorf("%w: reserved instruction", errBadDelta)
		}
	}
	if n != resultSize {
		return fmt.Errorf("%w: result is %d bytes, expected %d", errBadDelta, n, resultSize)
	}
	return nil
}
