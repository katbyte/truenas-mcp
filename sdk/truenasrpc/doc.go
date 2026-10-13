// Package truenasrpc is the connection a TrueNAS is talked to through: one websocket, logged in once, that any number of calls share. It is written
// by hand, where the calls themselves are generated, because how a TrueNAS is reached is not in the description its API gives of itself.
//
//	nas, err := truenasrpc.New("nas.example.net", truenasrpc.APIKey("metrics", key), truenasrpc.WithLog(clog.Log))
//	var info map[string]any
//	err = nas.Call(ctx, "system.info", nil, &info)
//
// Nothing is dialled until the first call, or Connect. A connection that went away is dialled again by the next call, and a NAS that cannot be
// reached is left alone for a while between tries. TrueNAS revokes an API key sent without TLS, so an address without it is refused.
//
// Calls that run for a long time (a scrub, a replication) answer when they are done, which is not how a TrueNAS behaves unasked: the connection asks
// for it when it logs in, so that every call answers what its description says it does. Start is for a job nobody wants to wait on: it answers a Job
// at once, to follow, wait for, stop, or find again by its number. Subscribe hears what the NAS says when something changes.
//
// Jobs that are started, and everything subscribed to, go over a second websocket that is dialled the first time either is used, so that neither
// takes any of the places calls are made in.
package truenasrpc
