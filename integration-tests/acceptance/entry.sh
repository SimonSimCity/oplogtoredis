#!/bin/sh

set -e
cd `dirname "$0"`

mongo "$MONGO_URL" --eval 'rs.initiate({ _id: "myapp", members: [{ _id: 0, host: "mongo:27017"}] })'
go test . -timeout 60s
