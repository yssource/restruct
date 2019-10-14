package restruct

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"

	"github.com/go-restruct/restruct/expr"
)

// Packer is a type capable of packing a native value into a binary
// representation. The Pack function is expected to overwrite a number of
// bytes in buf then return a slice of the remaining buffer. Note that you
// must also implement SizeOf, and returning an incorrect SizeOf will cause
// the encoder to crash. The SizeOf should be equal to the number of bytes
// consumed from the buffer slice in Pack. You may use a pointer receiver even
// if the type is used by value.
type Packer interface {
	Sizer
	Pack(buf []byte, order binary.ByteOrder) ([]byte, error)
}

type encoder struct {
	order      binary.ByteOrder
	buf        []byte
	struc      reflect.Value
	sfields    []field
	bitCounter int
	bitSize    int
	allowExpr  bool
	exprEnv    expr.Resolver
}

func (e *encoder) resolver() expr.Resolver {
	if e.exprEnv == nil {
		if !e.allowExpr {
			panic("call restruct.EnableExprBeta() to eanble expressions beta")
		}
		e.exprEnv = makeResolver(e.struc)
	}
	return e.exprEnv
}

func getBit(buf []byte, bitSize int, bit int) byte {
	bit = bitSize - 1 - bit
	return (buf[len(buf)-bit/8-1] >> (uint(bit) % 8)) & 1
}

func (e *encoder) writeBit(value byte) {
	e.buf[0] |= (value & 1) << uint(7-e.bitCounter)
	e.bitCounter++
	if e.bitCounter >= 8 {
		e.buf = e.buf[1:]
		e.bitCounter -= 8
	}
}

func (e *encoder) writeBits(f field, inBuf []byte) {
	var encodedBits int

	// Determine encoded size in bits.
	if e.bitSize == 0 {
		encodedBits = 8 * len(inBuf)
	} else {
		encodedBits = int(e.bitSize)
	}

	// Crop input buffer to relevant bytes only.
	inBuf = inBuf[len(inBuf)-(encodedBits+7)/8:]

	if e.bitCounter == 0 && encodedBits%8 == 0 {
		// Fast path: we are fully byte-aligned.
		copy(e.buf, inBuf)
		e.buf = e.buf[len(inBuf):]
	} else {
		// Slow path: work bit-by-bit.
		// TODO: This needs to be optimized in a way that can be easily
		// understood; the previous optimized version was simply too hard to
		// reason about.
		for i := 0; i < encodedBits; i++ {
			e.writeBit(getBit(inBuf, encodedBits, i))
		}
	}
}

func (e *encoder) write8(f field, x uint8) {
	b := make([]byte, 1)
	b[0] = x
	e.writeBits(f, b)
}

func (e *encoder) write16(f field, x uint16) {
	b := make([]byte, 2)
	e.order.PutUint16(b, x)
	e.writeBits(f, b)
}

func (e *encoder) write32(f field, x uint32) {
	b := make([]byte, 4)
	e.order.PutUint32(b, x)
	e.writeBits(f, b)
}

func (e *encoder) write64(f field, x uint64) {
	b := make([]byte, 8)
	e.order.PutUint64(b, x)
	e.writeBits(f, b)
}

func (e *encoder) writeS8(f field, x int8) { e.write8(f, uint8(x)) }

func (e *encoder) writeS16(f field, x int16) { e.write16(f, uint16(x)) }

func (e *encoder) writeS32(f field, x int32) { e.write32(f, uint32(x)) }

func (e *encoder) writeS64(f field, x int64) { e.write64(f, uint64(x)) }

func (e *encoder) skipBits(count int) {
	e.bitCounter += count % 8
	if e.bitCounter > 8 {
		e.bitCounter -= 8
		count += 8
	}
	e.buf = e.buf[count/8:]
}

func (e *encoder) skip(f field, v reflect.Value) {
	e.skipBits(f.SizeOfBits(v, e.struc))
}

func (e *encoder) packer(v reflect.Value) (Packer, bool) {
	if s, ok := v.Interface().(Packer); ok {
		return s, true
	}

	if !v.CanAddr() {
		return nil, false
	}

	if s, ok := v.Addr().Interface().(Packer); ok {
		return s, true
	}

	return nil, false
}

func (e *encoder) intFromField(f field, v reflect.Value) int64 {
	switch v.Kind() {
	case reflect.Bool:
		b := v.Bool()
		if f.Flags&InvertedBoolFlag == InvertedBoolFlag {
			b = !b
		}
		if b {
			if f.Flags&VariantBoolFlag == VariantBoolFlag {
				return -1
			}
			return 1
		}
		return 0
	default:
		return v.Int()
	}
}

func (e *encoder) uintFromField(f field, v reflect.Value) uint64 {
	switch v.Kind() {
	case reflect.Bool:
		b := v.Bool()
		if f.Flags&InvertedBoolFlag == InvertedBoolFlag {
			b = !b
		}
		if b {
			if f.Flags&VariantBoolFlag == VariantBoolFlag {
				return ^uint64(0)
			}
			return 1
		}
		return 0
	default:
		return v.Uint()
	}
}

func (e *encoder) write(f field, v reflect.Value) {
	if f.Name != "_" {
		if s, ok := e.packer(v); ok {
			var err error
			e.buf, err = s.Pack(e.buf, e.order)
			if err != nil {
				panic(err)
			}
			return
		}
	} else {
		e.skipBits(f.SizeOfBits(v, e.struc))
		return
	}

	if !evalIf(&f, e.resolver) {
		return
	}

	struc := e.struc
	sfields := e.sfields
	order := e.order

	if f.Order != nil {
		e.order = f.Order
		defer func() { e.order = order }()
	}

	if f.Skip != 0 {
		e.skipBits(f.Skip * 8)
	}

	e.bitSize = evalBits(&f, e.resolver)

	// If this is a sizeof field, pull the current slice length into it.
	if f.TIndex != -1 {
		sv := struc.Field(f.TIndex)

		switch f.BinaryType.Kind() {
		case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			v.SetInt(int64(sv.Len()))
		case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			v.SetUint(uint64(sv.Len()))
		default:
			panic(fmt.Errorf("unsupported size type %s: %s", f.BinaryType.String(), f.Name))
		}
	}

	ov := v
	if f.OutExpr != nil {
		out, err := expr.EvalProgram(e.resolver(), f.OutExpr)
		if err != nil {
			panic(fmt.Errorf("error processing out expr for %s: %v", f.Name, err))
		}
		ov = reflect.ValueOf(out)
	}

	switch f.BinaryType.Kind() {
	case reflect.Ptr:
		// Skip if pointer is nil.
		if v.IsNil() {
			return
		}

		e.write(f.Elem(), v.Elem())

	case reflect.Array, reflect.Slice, reflect.String:
		switch f.NativeType.Kind() {
		case reflect.Slice, reflect.String:
			if f.SizeExpr != nil {
				if l := evalSize(&f, e.resolver); l != ov.Len() {
					panic(fmt.Errorf("length does not match size expression (%d != %d)", ov.Len(), l))
				}
			}
			fallthrough
		case reflect.Array:
			ef := f.Elem()
			len := ov.Len()
			cap := len
			if f.BinaryType.Kind() == reflect.Array {
				cap = f.BinaryType.Len()
			}
			for i := 0; i < len; i++ {
				e.write(ef, ov.Index(i))
			}
			for i := len; i < cap; i++ {
				e.write(ef, reflect.New(f.BinaryType.Elem()).Elem())
			}
		default:
			panic(fmt.Errorf("invalid array cast type: %s", f.NativeType.String()))
		}

	case reflect.Struct:
		e.struc = ov
		e.sfields = cachedFieldsFromStruct(f.BinaryType)
		l := len(e.sfields)
		for i := 0; i < l; i++ {
			sf := e.sfields[i]
			sv := ov.Field(sf.Index)
			if sv.CanSet() {
				e.write(sf, sv)
			} else {
				e.skip(sf, sv)
			}
		}
		e.sfields = sfields
		e.struc = struc

	case reflect.Int8:
		e.writeS8(f, int8(e.intFromField(f, ov)))
	case reflect.Int16:
		e.writeS16(f, int16(e.intFromField(f, ov)))
	case reflect.Int32:
		e.writeS32(f, int32(e.intFromField(f, ov)))
	case reflect.Int64:
		e.writeS64(f, int64(e.intFromField(f, ov)))

	case reflect.Uint8, reflect.Bool:
		e.write8(f, uint8(e.uintFromField(f, ov)))
	case reflect.Uint16:
		e.write16(f, uint16(e.uintFromField(f, ov)))
	case reflect.Uint32:
		e.write32(f, uint32(e.uintFromField(f, ov)))
	case reflect.Uint64:
		e.write64(f, uint64(e.uintFromField(f, ov)))

	case reflect.Float32:
		e.write32(f, math.Float32bits(float32(ov.Float())))
	case reflect.Float64:
		e.write64(f, math.Float64bits(float64(ov.Float())))

	case reflect.Complex64:
		x := ov.Complex()
		e.write32(f, math.Float32bits(float32(real(x))))
		e.write32(f, math.Float32bits(float32(imag(x))))
	case reflect.Complex128:
		x := ov.Complex()
		e.write64(f, math.Float64bits(float64(real(x))))
		e.write64(f, math.Float64bits(float64(imag(x))))
	}
}
