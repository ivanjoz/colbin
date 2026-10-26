package wire

// Float columns: a float64 column is written through column.AppendFloat64,
// which makes it a decimal column when its values are decimals. The reader
// needs nothing new: column.DecodeArray reads either form back into the bit
// patterns a float column has always decoded to.

import "github.com/ivanjoz/colbin/column"

// FloatColumn is Column for a float64 column held as its bit patterns.
func (w *Writer8) FloatColumn(key uint8, values []int64) {
	if allZero(values) {
		return
	}
	mark := w.openSized(key, classCol, 0, len(values))
	w.Buffer = column.AppendFloat64(w.Buffer, values)
	w.Close(mark)
}

// FloatColumn is Writer8.FloatColumn with four key bits.
func (w *Writer) FloatColumn(key uint8, values []int64) {
	if allZero(values) {
		return
	}
	mark := w.openNarrowSized(key, 0, len(values))
	w.Buffer = column.AppendFloat64(w.Buffer, values)
	w.Close(mark)
}

// appendInts is the column codec a column of values takes: the float one for
// a float64 column's bit patterns.
func appendInts(out []byte, values []int64, float bool) []byte {
	if float {
		return column.AppendFloat64(out, values)
	}
	return column.AppendArray(out, values)
}
