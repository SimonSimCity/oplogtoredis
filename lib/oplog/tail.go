package oplog

import (
	"time"

	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/go-redis/redis"
	"github.com/tulip/oplogtoredis/lib/log"
	"github.com/tulip/oplogtoredis/lib/redispub"
)

// Tailer persistently tails the oplog of a Mongo cluster, handling
// reconnection and resumption of where it left off.
type Tailer struct {
	MongoClient *mgo.Session
	RedisClient redis.UniversalClient
	RedisPrefix string
	MaxCatchUp  time.Duration
}

// Raw oplog entry from Mongo
type rawOplogEntry struct {
	Timestamp    bson.MongoTimestamp    `bson:"ts"`
	HistoryID    int64                  `bson:"h"`
	MongoVersion int                    `bson:"v"`
	Operation    string                 `bson:"op"`
	Namespace    string                 `bson:"ns"`
	Doc          map[string]interface{} `bson:"o"`
	Update       rawOplogEntryID        `bson:"o2"`
}

type rawOplogEntryID struct {
	ID interface{} `bson:"_id"`
}

const requeryDuration = time.Second

// Tail begins tailing the oplog. It doesn't return unless it receives a message
// on the stop channel, in which case it wraps up its work and then returns.
func (tailer *Tailer) Tail(out chan<- *redispub.Publication, stop <-chan bool) {
	childStopC := make(chan bool)
	wasStopped := false

	go func() {
		<-stop
		wasStopped = true
		childStopC <- true
	}()

	for {
		log.Log.Info("Starting oplog tailing")
		tailer.tailOnce(out, childStopC)
		log.Log.Info("Oplog tailing ended")

		if wasStopped {
			return
		}

		log.Log.Errorw("Oplog tailing stopped prematurely. Waiting a second an then retrying.")
		time.Sleep(requeryDuration)
	}
}

func (tailer *Tailer) tailOnce(out chan<- *redispub.Publication, stop <-chan bool) {
	session := tailer.MongoClient.Copy()
	oplogCollection := session.DB("local").C("oplog.rs")

	startTime := tailer.getStartTime(func() (bson.MongoTimestamp, error) {
		// Get the timestamp of the last entry in the oplog (as a position to
		// start from if we don't have a last-written timestamp from Redis)
		var entry oplogEntry
		mongoErr := session.DB("local").C("oplog.rs").Find(nil).Sort("-$natural").One(&entry)

		return entry.Timestamp, mongoErr
	})

	query := oplogCollection.Find(bson.M{"ts": bson.M{"$gt": startTime}})
	iter := query.LogReplay().Sort("$natural").Tail(requeryDuration)

	var lastTimestamp bson.MongoTimestamp
	for {
		select {
		case <-stop:
			log.Log.Infof("Received stop; aborting oplog tailing")
			return
		default:
		}

		var result rawOplogEntry

		for iter.Next(&result) {
			lastTimestamp = result.Timestamp

			entry := tailer.parseRawOplogEntry(&result)
			if entry == nil {
				continue
			}

			log.Log.Debugw("Received oplog entry",
				"entry", entry)

			pub := processOplogEntry(entry)
			if pub == nil {
				continue
			}

			out <- pub
		}

		if iter.Err() != nil {
			log.Log.Errorw("Error from oplog iterator",
				"error", iter.Err())

			closeErr := iter.Close()
			if closeErr != nil {
				log.Log.Errorw("Error from closing oplog iterator",
					"error", closeErr)
			}

			return
		}

		if iter.Timeout() {
			// Didn't get any messages for a while, keep trying
			log.Log.Warn("Oplog cursor timed out, will retry")
			continue
		}

		// Our cursor expired. Make a new cursor to pick up from where we
		// left off.
		query := oplogCollection.Find(bson.M{"ts": bson.M{"$gt": lastTimestamp}})
		iter = query.LogReplay().Sort("$natural").Tail(requeryDuration)
	}
}

// Gets the bson.MongoTimestamp from which we should start tailing
//
// We take the function to get the timestamp of the last oplog entry (as a
// fallback if we don't have a latest timestamp from Redis) as an arg instead
// of using tailer.mongoClient directly so we can unit test this function
func (tailer *Tailer) getStartTime(getTimestampOfLastOplogEntry func() (bson.MongoTimestamp, error)) bson.MongoTimestamp {
	ts, tsTime, redisErr := redispub.LastProcessedTimestamp(tailer.RedisClient, tailer.RedisPrefix)

	if redisErr == nil {
		// we have a last write time, check that it's not too far in the
		// past
		if tsTime.After(time.Now().Add(-1 * tailer.MaxCatchUp)) {
			log.Log.Infof("Found last processed timestamp, resuming oplog tailing from %d", tsTime.Unix())
			return ts
		}

		log.Log.Warnf("Found last processed timestamp, but it was too far in the past (%d). Will start from end of oplog", tsTime.Unix())
	}

	if redisErr != redis.Nil {
		log.Log.Errorw("Error querying Redis for last processed timestamp. Will start from end of oplog.",
			"error", redisErr)
	}

	mongoOplogEndTimestamp, mongoErr := getTimestampOfLastOplogEntry()
	if mongoErr == nil {
		log.Log.Infof("Starting tailing from end of oplog (timestamp %d)", int64(mongoOplogEndTimestamp))
		return mongoOplogEndTimestamp
	}

	log.Log.Errorw("Got error when asking for last operation timestamp in the oplog. Returning current time.",
		"error", mongoErr)
	return bson.MongoTimestamp(time.Now().Unix() << 32)
}

// converts a rawOplogEntry to an oplogEntry
func (tailer *Tailer) parseRawOplogEntry(rawEntry *rawOplogEntry) *oplogEntry {
	entry := oplogEntry{
		Operation: rawEntry.Operation,
		Timestamp: rawEntry.Timestamp,
		Namespace: rawEntry.Namespace,
		Data:      rawEntry.Doc,
	}

	if !(entry.IsInsert() || entry.IsUpdate() || entry.IsRemove()) {
		// discard commands like dropDatabase, etc.
		return nil
	}

	if rawEntry.Operation == operationUpdate {
		entry.DocID = rawEntry.Update.ID
	} else {
		entry.DocID = rawEntry.Doc["_id"]
	}

	return &entry
}