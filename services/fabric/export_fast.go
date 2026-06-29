package fabric

import "strconv"

type appendJSONPayload interface {
	AppendJSON([]byte) []byte
}

func appendPubAppendJSON(dst []byte, item exportItem, payload appendJSONPayload) ([]byte, bool) {
	dst = append(dst, `{"type":"pub","topic":`...)
	var ok bool
	dst, ok = appendExportTopicJSON(dst, item.topic)
	if !ok {
		return nil, false
	}
	dst = append(dst, `,"payload":`...)
	dst = payload.AppendJSON(dst)
	dst = append(dst, `,"retain":`...)
	dst = strconv.AppendBool(dst, item.retained)
	dst = append(dst, '}')
	return append(dst, '\n'), true
}

func marshalPubAppendJSON(item exportItem, payload appendJSONPayload) ([]byte, bool) {
	return appendPubAppendJSON(make([]byte, 0, 160), item, payload)
}

func appendUnretainTopic(dst []byte, topicItem exportItem) ([]byte, bool) {
	dst = append(dst, `{"type":"unretain","topic":`...)
	var ok bool
	dst, ok = appendExportTopicJSON(dst, topicItem.topic)
	if !ok {
		return nil, false
	}
	dst = append(dst, '}')
	return append(dst, '\n'), true
}

func marshalUnretainTopic(topicItem exportItem) ([]byte, bool) {
	return appendUnretainTopic(make([]byte, 0, 96), topicItem)
}
