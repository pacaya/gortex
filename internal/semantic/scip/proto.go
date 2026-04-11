package scip

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// protoReader is a minimal protobuf wire format decoder.
// It handles the subset of protobuf used by the SCIP schema
// without requiring the full protobuf dependency.
type protoReader struct {
	data []byte
	pos  int
}

func (r *protoReader) hasMore() bool {
	return r.pos < len(r.data)
}

func (r *protoReader) readVarint() (uint64, error) {
	if r.pos >= len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}

	var val uint64
	var shift uint
	for {
		if r.pos >= len(r.data) {
			return 0, io.ErrUnexpectedEOF
		}
		b := r.data[r.pos]
		r.pos++
		val |= uint64(b&0x7F) << shift
		if b < 0x80 {
			break
		}
		shift += 7
		if shift >= 64 {
			return 0, fmt.Errorf("varint too long")
		}
	}
	return val, nil
}

func (r *protoReader) readTag() (fieldNum int, wireType int, err error) {
	v, err := r.readVarint()
	if err != nil {
		return 0, 0, err
	}
	fieldNum = int(v >> 3)
	wireType = int(v & 0x7)
	return fieldNum, wireType, nil
}

func (r *protoReader) readBytes(wireType int) ([]byte, error) {
	if wireType != 2 {
		return nil, fmt.Errorf("expected wire type 2 (length-delimited), got %d", wireType)
	}
	length, err := r.readVarint()
	if err != nil {
		return nil, err
	}
	if r.pos+int(length) > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	data := r.data[r.pos : r.pos+int(length)]
	r.pos += int(length)
	return data, nil
}

func (r *protoReader) readString(wireType int) (string, error) {
	data, err := r.readBytes(wireType)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *protoReader) skipField(wireType int) error {
	switch wireType {
	case 0: // varint
		_, err := r.readVarint()
		return err
	case 1: // 64-bit
		if r.pos+8 > len(r.data) {
			return io.ErrUnexpectedEOF
		}
		r.pos += 8
		return nil
	case 2: // length-delimited
		length, err := r.readVarint()
		if err != nil {
			return err
		}
		if r.pos+int(length) > len(r.data) {
			return io.ErrUnexpectedEOF
		}
		r.pos += int(length)
		return nil
	case 5: // 32-bit
		if r.pos+4 > len(r.data) {
			return io.ErrUnexpectedEOF
		}
		r.pos += 4
		return nil
	default:
		return fmt.Errorf("unknown wire type %d", wireType)
	}
}

// encodeSCIPForTesting creates a minimal protobuf-encoded SCIP index for tests.
// This is used when we don't want to depend on the full protobuf library.
func encodeSCIPForTesting(index *SCIPIndex) []byte {
	var buf []byte

	for _, doc := range index.Documents {
		docBytes := encodeSCIPDocumentForTesting(&doc)
		buf = appendTag(buf, 2, 2) // field 2, wire type 2
		buf = appendBytes(buf, docBytes)
	}

	return buf
}

func encodeSCIPDocumentForTesting(doc *SCIPDocument) []byte {
	var buf []byte

	// field 4: relative_path
	if doc.RelativePath != "" {
		buf = appendTag(buf, 4, 2)
		buf = appendBytes(buf, []byte(doc.RelativePath))
	}

	// field 2: occurrences
	for _, occ := range doc.Occurrences {
		occBytes := encodeSCIPOccurrenceForTesting(&occ)
		buf = appendTag(buf, 2, 2)
		buf = appendBytes(buf, occBytes)
	}

	// field 3: symbols
	for _, sym := range doc.Symbols {
		symBytes := encodeSCIPSymbolInfoForTesting(&sym)
		buf = appendTag(buf, 3, 2)
		buf = appendBytes(buf, symBytes)
	}

	return buf
}

func encodeSCIPOccurrenceForTesting(occ *SCIPOccurrence) []byte {
	var buf []byte

	// field 1: range (packed repeated int32)
	if len(occ.Range) > 0 {
		var rangeBuf []byte
		for _, v := range occ.Range {
			rangeBuf = binary.AppendUvarint(rangeBuf, uint64(v))
		}
		buf = appendTag(buf, 1, 2) // packed
		buf = appendBytes(buf, rangeBuf)
	}

	// field 2: symbol
	if occ.Symbol != "" {
		buf = appendTag(buf, 2, 2)
		buf = appendBytes(buf, []byte(occ.Symbol))
	}

	// field 3: symbol_roles
	if occ.SymbolRoles != 0 {
		buf = appendTag(buf, 3, 0)
		buf = binary.AppendUvarint(buf, uint64(occ.SymbolRoles))
	}

	return buf
}

func encodeSCIPSymbolInfoForTesting(sym *SCIPSymbolInfo) []byte {
	var buf []byte

	// field 1: symbol
	if sym.Symbol != "" {
		buf = appendTag(buf, 1, 2)
		buf = appendBytes(buf, []byte(sym.Symbol))
	}

	// field 3: documentation
	for _, doc := range sym.Documentation {
		buf = appendTag(buf, 3, 2)
		buf = appendBytes(buf, []byte(doc))
	}

	// field 4: relationships
	for _, rel := range sym.Relationships {
		relBytes := encodeSCIPRelationshipForTesting(&rel)
		buf = appendTag(buf, 4, 2)
		buf = appendBytes(buf, relBytes)
	}

	return buf
}

func encodeSCIPRelationshipForTesting(rel *SCIPRelationship) []byte {
	var buf []byte

	if rel.Symbol != "" {
		buf = appendTag(buf, 1, 2)
		buf = appendBytes(buf, []byte(rel.Symbol))
	}
	if rel.IsImplementation {
		buf = appendTag(buf, 2, 0)
		buf = binary.AppendUvarint(buf, 1)
	}

	return buf
}

func appendTag(buf []byte, fieldNum, wireType int) []byte {
	return binary.AppendUvarint(buf, uint64(fieldNum<<3|wireType))
}

func appendBytes(buf, data []byte) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(data)))
	return append(buf, data...)
}

// Ensure math is used (prevents import error if needed).
var _ = math.MaxFloat64
