package message

import "github.com/ZhenForest/xsbot-core/sdk"

// Weight estimates retained content and container overhead for admission. These
// are conservative budget units, not an exact measurement of the Go heap.
func Weight(m sdk.Message) int64 {
	n := int64(512 + 2*(len(m.Sender.Nickname.Value)+len(m.Sender.Card.Value)))
	n += scalarWeight(m.Metadata)
	for _, s := range m.Segments {
		n += int64(384 + 2*(len(s.Text)+len(s.Payload)))
		if s.Resource != nil {
			n += int64(32 + 2*len(s.Resource.Token))
		}
		if s.Unknown != nil {
			n += int64(64 + 2*(len(s.Unknown.Reason)+len(s.Unknown.Location)))
		}
		n += scalarWeight(s.Metadata)
		if s.Quoted != nil {
			n += Weight(*s.Quoted)
		}
		for _, child := range s.Messages {
			n += Weight(child)
		}
	}
	return n
}

func scalarWeight(values map[string]sdk.Scalar) int64 {
	n := int64(64)
	for key, v := range values {
		n += int64(64+2*len(key)) + valueWeight(v)
	}
	return n
}
func valueWeight(v sdk.Scalar) int64 {
	n := int64(128)
	if v.String != nil {
		n += int64(2 * len(*v.String))
	}
	if v.Object != nil {
		n += scalarWeight(v.Object)
	}
	for _, x := range v.List {
		n += valueWeight(x)
	}
	return n
}
