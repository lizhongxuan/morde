package personal

import "encoding/json"

type Object struct {
	ObjectType string
	ObjectID   string
	Revision   int64
	Body       json.RawMessage
}
