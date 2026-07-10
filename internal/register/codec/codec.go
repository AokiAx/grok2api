package codec

import (
	"encoding/binary"
	"regexp"
	"strings"
)

var ActionIDPattern = regexp.MustCompile(`7f[a-fA-F0-9]{40}`)

func EncodeVarint(value int) []byte {
	if value < 0 {
		value = 0
	}
	result := make([]byte, 0, 5)
	for value > 0x7f {
		result = append(result, byte(value&0x7f)|0x80)
		value >>= 7
	}
	result = append(result, byte(value&0x7f))
	return result
}

func EncodeStringField(fieldNumber int, value string) []byte {
	tag := (fieldNumber << 3) | 2
	data := []byte(value)
	out := make([]byte, 0, len(EncodeVarint(tag))+len(EncodeVarint(len(data)))+len(data))
	out = append(out, EncodeVarint(tag)...)
	out = append(out, EncodeVarint(len(data))...)
	out = append(out, data...)
	return out
}

func WrapGRPCWeb(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0x00
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

type GRPCWebResponse struct {
	Status   string
	Payload  []byte
	Trailers map[string]string
}

func ParseGRPCWebResponse(data []byte) GRPCWebResponse {
	result := GRPCWebResponse{Trailers: map[string]string{}}
	pos := 0
	for pos+5 <= len(data) {
		flag := data[pos]
		length := int(binary.BigEndian.Uint32(data[pos+1 : pos+5]))
		pos += 5
		if pos+length > len(data) {
			break
		}
		frame := data[pos : pos+length]
		pos += length
		switch flag {
		case 0x80:
			for _, line := range strings.Split(strings.TrimSpace(string(frame)), "\r\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				result.Trailers[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
			if status, ok := result.Trailers["grpc-status"]; ok {
				result.Status = status
			}
		case 0x00:
			result.Payload = append([]byte(nil), frame...)
		}
	}
	return result
}

func ExtractServerActionIDFromJS(jsText string) string {
	match := ActionIDPattern.FindString(jsText)
	return match
}

func ExtractServerActionIDFromHTML(pageHTML string) string {
	match := regexp.MustCompile(`"(7f[a-fA-F0-9]{40})"`).FindStringSubmatch(pageHTML)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}
