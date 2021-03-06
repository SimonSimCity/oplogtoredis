package oplog

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/globalsign/mgo/bson"
	"github.com/tulip/oplogtoredis/lib/redispub"
)

// nolint: gocyclo
func TestProcessOplogEntry(t *testing.T) {
	// We can't compare raw publications because they contain JSON that can
	// be ordered differently. We have this decodedPublication type that's
	// the same as redispub.Publication but with the JSON decoded
	type decodedPublicationMessage struct {
		Event  string      `json:"e"`
		Doc    interface{} `json:"d"`
		Fields []string    `json:"f"`
	}
	type decodedPublication struct {
		CollectionChannel string
		SpecificChannel   string
		Msg               decodedPublicationMessage
		OplogTimestamp    bson.MongoTimestamp
	}

	tests := map[string]struct {
		// The oplogEntry to send to the tailer
		in *oplogEntry

		// The redispub.Publication we expect the tailer to produce. If the
		// test expects nothing to be published for the op, set this to nil
		want *decodedPublication

		wantError error
	}{
		"Basic insert": {
			in: &oplogEntry{
				DocID:      "someid",
				Operation:  "i",
				Namespace:  "foo.bar",
				Database:   "foo",
				Collection: "bar",
				Data: bson.M{
					"some": "field",
				},
				Timestamp: bson.MongoTimestamp(1234),
			},
			want: &decodedPublication{
				CollectionChannel: "foo.bar",
				SpecificChannel:   "foo.bar::someid",
				Msg: decodedPublicationMessage{
					Event: "i",
					Doc: map[string]interface{}{
						"_id": "someid",
					},
					Fields: []string{"some"},
				},
				OplogTimestamp: bson.MongoTimestamp(1234),
			},
		},
		"Replacement update": {
			in: &oplogEntry{
				DocID:      "someid",
				Operation:  "u",
				Namespace:  "foo.bar",
				Database:   "foo",
				Collection: "bar",
				Data: bson.M{
					"some": "field",
					"new":  "field",
				},
				Timestamp: bson.MongoTimestamp(1234),
			},
			want: &decodedPublication{
				CollectionChannel: "foo.bar",
				SpecificChannel:   "foo.bar::someid",
				Msg: decodedPublicationMessage{
					Event: "u",
					Doc: map[string]interface{}{
						"_id": "someid",
					},
					Fields: []string{"some", "new"},
				},
				OplogTimestamp: bson.MongoTimestamp(1234),
			},
		},
		"Non-replacement update": {
			in: &oplogEntry{
				DocID:      "someid",
				Operation:  "u",
				Namespace:  "foo.bar",
				Database:   "foo",
				Collection: "bar",
				Data: bson.M{
					"$v": "1.2.3",
					"$set": map[string]interface{}{
						"a": "foo",
						"b": "foo",
					},
					"$unset": map[string]interface{}{
						"c": "foo",
					},
				},
				Timestamp: bson.MongoTimestamp(1234),
			},
			want: &decodedPublication{
				CollectionChannel: "foo.bar",
				SpecificChannel:   "foo.bar::someid",
				Msg: decodedPublicationMessage{
					Event: "u",
					Doc: map[string]interface{}{
						"_id": "someid",
					},
					Fields: []string{"a", "b", "c"},
				},
				OplogTimestamp: bson.MongoTimestamp(1234),
			},
		},
		"Delete": {
			in: &oplogEntry{
				DocID:      "someid",
				Operation:  "d",
				Namespace:  "foo.bar",
				Database:   "foo",
				Collection: "bar",
				Data:       bson.M{},
				Timestamp:  bson.MongoTimestamp(1234),
			},
			want: &decodedPublication{
				CollectionChannel: "foo.bar",
				SpecificChannel:   "foo.bar::someid",
				Msg: decodedPublicationMessage{
					Event: "r",
					Doc: map[string]interface{}{
						"_id": "someid",
					},
					Fields: []string{},
				},
				OplogTimestamp: bson.MongoTimestamp(1234),
			},
		},
		"ObjectID id": {
			in: &oplogEntry{
				DocID:      bson.ObjectIdHex("deadbeefdeadbeefdeadbeef"),
				Operation:  "i",
				Namespace:  "foo.bar",
				Database:   "foo",
				Collection: "bar",
				Data: bson.M{
					"some": "field",
				},
				Timestamp: bson.MongoTimestamp(1234),
			},
			want: &decodedPublication{
				CollectionChannel: "foo.bar",
				SpecificChannel:   "foo.bar::deadbeefdeadbeefdeadbeef",
				Msg: decodedPublicationMessage{
					Event: "i",
					Doc: map[string]interface{}{
						"_id": map[string]interface{}{
							"$type":  "oid",
							"$value": "deadbeefdeadbeefdeadbeef",
						},
					},
					Fields: []string{"some"},
				},
				OplogTimestamp: bson.MongoTimestamp(1234),
			},
		},
		"Unsupported id type": {
			in: &oplogEntry{
				DocID:      1234,
				Operation:  "i",
				Namespace:  "foo.bar",
				Database:   "foo",
				Collection: "bar",
				Data: bson.M{
					"some": "field",
				},
				Timestamp: bson.MongoTimestamp(1234),
			},
			wantError: errors.New("op.ID was not a string or ObjectID"),
			want:      nil,
		},
		"Index update": {
			in: &oplogEntry{
				DocID:      "someid",
				Operation:  "i",
				Namespace:  "foo.system.indexes",
				Database:   "foo",
				Collection: "system.indexes",
				Data: bson.M{
					"some": "field",
				},
				Timestamp: bson.MongoTimestamp(1234),
			},
			want: nil,
		},
	}

	// helper to convert a redispub.Publication to a decodedPublication
	decodePublication := func(pub *redispub.Publication) *decodedPublication {
		msg := decodedPublicationMessage{}
		err := json.Unmarshal(pub.Msg, &msg)
		if err != nil {
			panic(fmt.Sprintf("Error parsing Msg field of publication: %s\n    JSON: %s",
				err, pub.Msg))
		}

		return &decodedPublication{
			CollectionChannel: pub.CollectionChannel,
			SpecificChannel:   pub.SpecificChannel,
			Msg:               msg,
			OplogTimestamp:    pub.OplogTimestamp,
		}
	}

	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			// Create an output channel. We create a buffered channel so that
			// we can run Tail

			got, err := processOplogEntry(test.in)

			if test.wantError != err {
				if (err != nil) && (test.wantError == nil) {
					t.Fatalf("Got an unexpected error: %s", err)
				}

				if (err == nil) && (test.wantError != nil) {
					t.Fatal("Did not get an error, but expected one")
				}

				if test.wantError.Error() != err.Error() {
					t.Fatalf("Got wrong error: %s; expected error: %s", err, test.wantError)
				}

				return
			}

			if (got == nil) && (test.want != nil) {
				t.Errorf("Got nil when we expected a publication\n    Input: %#v\n    Wanted: %#v",
					test.in, test.want)
			} else if (got != nil) && (test.want == nil) {
				t.Errorf("Got a publication when we expected nil\n    Input: %#v\n    Got: %#v",
					test.in, got)
			} else if (got != nil) && (test.want != nil) {
				decodedGot := decodePublication(got)

				// sort the array of fields so we can compare them
				sort.Strings(test.want.Msg.Fields)
				sort.Strings(decodedGot.Msg.Fields)

				if !reflect.DeepEqual(decodedGot, test.want) {
					t.Errorf("Got incorrect publication\n    Got: %#v\n    Want: %#v",
						decodedGot, test.want)
				}
			}
		})
	}
}
