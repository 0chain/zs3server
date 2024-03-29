package cmd

// Code generated by github.com/tinylib/msgp DO NOT EDIT.

import (
	"github.com/tinylib/msgp/msgp"
)

// DecodeMsg implements msgp.Decodable
func (z *AccElem) DecodeMsg(dc *msgp.Reader) (err error) {
	var field []byte
	_ = field
	var zb0001 uint32
	zb0001, err = dc.ReadMapHeader()
	if err != nil {
		err = msgp.WrapError(err)
		return
	}
	for zb0001 > 0 {
		zb0001--
		field, err = dc.ReadMapKeyPtr()
		if err != nil {
			err = msgp.WrapError(err)
			return
		}
		switch msgp.UnsafeString(field) {
		case "Total":
			z.Total, err = dc.ReadInt64()
			if err != nil {
				err = msgp.WrapError(err, "Total")
				return
			}
		case "N":
			z.N, err = dc.ReadInt64()
			if err != nil {
				err = msgp.WrapError(err, "N")
				return
			}
		default:
			err = dc.Skip()
			if err != nil {
				err = msgp.WrapError(err)
				return
			}
		}
	}
	return
}

// EncodeMsg implements msgp.Encodable
func (z AccElem) EncodeMsg(en *msgp.Writer) (err error) {
	// map header, size 2
	// write "Total"
	err = en.Append(0x82, 0xa5, 0x54, 0x6f, 0x74, 0x61, 0x6c)
	if err != nil {
		return
	}
	err = en.WriteInt64(z.Total)
	if err != nil {
		err = msgp.WrapError(err, "Total")
		return
	}
	// write "N"
	err = en.Append(0xa1, 0x4e)
	if err != nil {
		return
	}
	err = en.WriteInt64(z.N)
	if err != nil {
		err = msgp.WrapError(err, "N")
		return
	}
	return
}

// MarshalMsg implements msgp.Marshaler
func (z AccElem) MarshalMsg(b []byte) (o []byte, err error) {
	o = msgp.Require(b, z.Msgsize())
	// map header, size 2
	// string "Total"
	o = append(o, 0x82, 0xa5, 0x54, 0x6f, 0x74, 0x61, 0x6c)
	o = msgp.AppendInt64(o, z.Total)
	// string "N"
	o = append(o, 0xa1, 0x4e)
	o = msgp.AppendInt64(o, z.N)
	return
}

// UnmarshalMsg implements msgp.Unmarshaler
func (z *AccElem) UnmarshalMsg(bts []byte) (o []byte, err error) {
	var field []byte
	_ = field
	var zb0001 uint32
	zb0001, bts, err = msgp.ReadMapHeaderBytes(bts)
	if err != nil {
		err = msgp.WrapError(err)
		return
	}
	for zb0001 > 0 {
		zb0001--
		field, bts, err = msgp.ReadMapKeyZC(bts)
		if err != nil {
			err = msgp.WrapError(err)
			return
		}
		switch msgp.UnsafeString(field) {
		case "Total":
			z.Total, bts, err = msgp.ReadInt64Bytes(bts)
			if err != nil {
				err = msgp.WrapError(err, "Total")
				return
			}
		case "N":
			z.N, bts, err = msgp.ReadInt64Bytes(bts)
			if err != nil {
				err = msgp.WrapError(err, "N")
				return
			}
		default:
			bts, err = msgp.Skip(bts)
			if err != nil {
				err = msgp.WrapError(err)
				return
			}
		}
	}
	o = bts
	return
}

// Msgsize returns an upper bound estimate of the number of bytes occupied by the serialized message
func (z AccElem) Msgsize() (s int) {
	s = 1 + 6 + msgp.Int64Size + 2 + msgp.Int64Size
	return
}

// DecodeMsg implements msgp.Decodable
func (z *LastMinuteLatencies) DecodeMsg(dc *msgp.Reader) (err error) {
	var field []byte
	_ = field
	var zb0001 uint32
	zb0001, err = dc.ReadMapHeader()
	if err != nil {
		err = msgp.WrapError(err)
		return
	}
	for zb0001 > 0 {
		zb0001--
		field, err = dc.ReadMapKeyPtr()
		if err != nil {
			err = msgp.WrapError(err)
			return
		}
		switch msgp.UnsafeString(field) {
		case "Totals":
			var zb0002 uint32
			zb0002, err = dc.ReadArrayHeader()
			if err != nil {
				err = msgp.WrapError(err, "Totals")
				return
			}
			if zb0002 != uint32(60) {
				err = msgp.ArrayError{Wanted: uint32(60), Got: zb0002}
				return
			}
			for za0001 := range z.Totals {
				var zb0003 uint32
				zb0003, err = dc.ReadArrayHeader()
				if err != nil {
					err = msgp.WrapError(err, "Totals", za0001)
					return
				}
				if zb0003 != uint32(sizeLastElemMarker) {
					err = msgp.ArrayError{Wanted: uint32(sizeLastElemMarker), Got: zb0003}
					return
				}
				for za0002 := range z.Totals[za0001] {
					var zb0004 uint32
					zb0004, err = dc.ReadMapHeader()
					if err != nil {
						err = msgp.WrapError(err, "Totals", za0001, za0002)
						return
					}
					for zb0004 > 0 {
						zb0004--
						field, err = dc.ReadMapKeyPtr()
						if err != nil {
							err = msgp.WrapError(err, "Totals", za0001, za0002)
							return
						}
						switch msgp.UnsafeString(field) {
						case "Total":
							z.Totals[za0001][za0002].Total, err = dc.ReadInt64()
							if err != nil {
								err = msgp.WrapError(err, "Totals", za0001, za0002, "Total")
								return
							}
						case "N":
							z.Totals[za0001][za0002].N, err = dc.ReadInt64()
							if err != nil {
								err = msgp.WrapError(err, "Totals", za0001, za0002, "N")
								return
							}
						default:
							err = dc.Skip()
							if err != nil {
								err = msgp.WrapError(err, "Totals", za0001, za0002)
								return
							}
						}
					}
				}
			}
		case "LastSec":
			z.LastSec, err = dc.ReadInt64()
			if err != nil {
				err = msgp.WrapError(err, "LastSec")
				return
			}
		default:
			err = dc.Skip()
			if err != nil {
				err = msgp.WrapError(err)
				return
			}
		}
	}
	return
}

// EncodeMsg implements msgp.Encodable
func (z *LastMinuteLatencies) EncodeMsg(en *msgp.Writer) (err error) {
	// map header, size 2
	// write "Totals"
	err = en.Append(0x82, 0xa6, 0x54, 0x6f, 0x74, 0x61, 0x6c, 0x73)
	if err != nil {
		return
	}
	err = en.WriteArrayHeader(uint32(60))
	if err != nil {
		err = msgp.WrapError(err, "Totals")
		return
	}
	for za0001 := range z.Totals {
		err = en.WriteArrayHeader(uint32(sizeLastElemMarker))
		if err != nil {
			err = msgp.WrapError(err, "Totals", za0001)
			return
		}
		for za0002 := range z.Totals[za0001] {
			// map header, size 2
			// write "Total"
			err = en.Append(0x82, 0xa5, 0x54, 0x6f, 0x74, 0x61, 0x6c)
			if err != nil {
				return
			}
			err = en.WriteInt64(z.Totals[za0001][za0002].Total)
			if err != nil {
				err = msgp.WrapError(err, "Totals", za0001, za0002, "Total")
				return
			}
			// write "N"
			err = en.Append(0xa1, 0x4e)
			if err != nil {
				return
			}
			err = en.WriteInt64(z.Totals[za0001][za0002].N)
			if err != nil {
				err = msgp.WrapError(err, "Totals", za0001, za0002, "N")
				return
			}
		}
	}
	// write "LastSec"
	err = en.Append(0xa7, 0x4c, 0x61, 0x73, 0x74, 0x53, 0x65, 0x63)
	if err != nil {
		return
	}
	err = en.WriteInt64(z.LastSec)
	if err != nil {
		err = msgp.WrapError(err, "LastSec")
		return
	}
	return
}

// MarshalMsg implements msgp.Marshaler
func (z *LastMinuteLatencies) MarshalMsg(b []byte) (o []byte, err error) {
	o = msgp.Require(b, z.Msgsize())
	// map header, size 2
	// string "Totals"
	o = append(o, 0x82, 0xa6, 0x54, 0x6f, 0x74, 0x61, 0x6c, 0x73)
	o = msgp.AppendArrayHeader(o, uint32(60))
	for za0001 := range z.Totals {
		o = msgp.AppendArrayHeader(o, uint32(sizeLastElemMarker))
		for za0002 := range z.Totals[za0001] {
			// map header, size 2
			// string "Total"
			o = append(o, 0x82, 0xa5, 0x54, 0x6f, 0x74, 0x61, 0x6c)
			o = msgp.AppendInt64(o, z.Totals[za0001][za0002].Total)
			// string "N"
			o = append(o, 0xa1, 0x4e)
			o = msgp.AppendInt64(o, z.Totals[za0001][za0002].N)
		}
	}
	// string "LastSec"
	o = append(o, 0xa7, 0x4c, 0x61, 0x73, 0x74, 0x53, 0x65, 0x63)
	o = msgp.AppendInt64(o, z.LastSec)
	return
}

// UnmarshalMsg implements msgp.Unmarshaler
func (z *LastMinuteLatencies) UnmarshalMsg(bts []byte) (o []byte, err error) {
	var field []byte
	_ = field
	var zb0001 uint32
	zb0001, bts, err = msgp.ReadMapHeaderBytes(bts)
	if err != nil {
		err = msgp.WrapError(err)
		return
	}
	for zb0001 > 0 {
		zb0001--
		field, bts, err = msgp.ReadMapKeyZC(bts)
		if err != nil {
			err = msgp.WrapError(err)
			return
		}
		switch msgp.UnsafeString(field) {
		case "Totals":
			var zb0002 uint32
			zb0002, bts, err = msgp.ReadArrayHeaderBytes(bts)
			if err != nil {
				err = msgp.WrapError(err, "Totals")
				return
			}
			if zb0002 != uint32(60) {
				err = msgp.ArrayError{Wanted: uint32(60), Got: zb0002}
				return
			}
			for za0001 := range z.Totals {
				var zb0003 uint32
				zb0003, bts, err = msgp.ReadArrayHeaderBytes(bts)
				if err != nil {
					err = msgp.WrapError(err, "Totals", za0001)
					return
				}
				if zb0003 != uint32(sizeLastElemMarker) {
					err = msgp.ArrayError{Wanted: uint32(sizeLastElemMarker), Got: zb0003}
					return
				}
				for za0002 := range z.Totals[za0001] {
					var zb0004 uint32
					zb0004, bts, err = msgp.ReadMapHeaderBytes(bts)
					if err != nil {
						err = msgp.WrapError(err, "Totals", za0001, za0002)
						return
					}
					for zb0004 > 0 {
						zb0004--
						field, bts, err = msgp.ReadMapKeyZC(bts)
						if err != nil {
							err = msgp.WrapError(err, "Totals", za0001, za0002)
							return
						}
						switch msgp.UnsafeString(field) {
						case "Total":
							z.Totals[za0001][za0002].Total, bts, err = msgp.ReadInt64Bytes(bts)
							if err != nil {
								err = msgp.WrapError(err, "Totals", za0001, za0002, "Total")
								return
							}
						case "N":
							z.Totals[za0001][za0002].N, bts, err = msgp.ReadInt64Bytes(bts)
							if err != nil {
								err = msgp.WrapError(err, "Totals", za0001, za0002, "N")
								return
							}
						default:
							bts, err = msgp.Skip(bts)
							if err != nil {
								err = msgp.WrapError(err, "Totals", za0001, za0002)
								return
							}
						}
					}
				}
			}
		case "LastSec":
			z.LastSec, bts, err = msgp.ReadInt64Bytes(bts)
			if err != nil {
				err = msgp.WrapError(err, "LastSec")
				return
			}
		default:
			bts, err = msgp.Skip(bts)
			if err != nil {
				err = msgp.WrapError(err)
				return
			}
		}
	}
	o = bts
	return
}

// Msgsize returns an upper bound estimate of the number of bytes occupied by the serialized message
func (z *LastMinuteLatencies) Msgsize() (s int) {
	s = 1 + 7 + msgp.ArrayHeaderSize + (60 * (sizeLastElemMarker * (9 + msgp.Int64Size + msgp.Int64Size))) + 8 + msgp.Int64Size
	return
}
