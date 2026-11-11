// Package actions wires the Neo4j client onto the plugin's node actions: two
// Cypher actions (a read and a write), three graph actions that need no Cypher
// written by hand, and a schema action — plus the meta RPCs the node's forms
// use.
package actions

import (
	"context"
	"fmt"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/neo4j-plugin/internal/neo4j"
)

// jobTimeout bounds one action's database traffic. It is well under any
// realistic flow timeout, so a hung server fails the node instead of the flow.
const jobTimeout = 2 * time.Minute

// Registry owns what the actions share: the pool that turns each call's
// settings profile into a client. The plugin holds no database configuration of
// its own.
type Registry struct {
	pool *neo4j.Pool
}

func New() *Registry { return &Registry{pool: neo4j.NewPool()} }

// connection is the platform-managed half of every request body. The runtime
// resolves the settings profile bound to the node and folds it into the call as
// `body.settings`, so actions declare only their own fields.
type connection struct {
	Settings map[string]any `json:"settings"`
}

// clientFor resolves the connection a request carries. It reads the same bytes
// the action's own input came from, so the settings envelope stays out of every
// action's input struct.
func (r *Registry) clientFor(data []byte) (*neo4j.Client, error) {
	conn, err := sdkv1.CastRequestTo[connection](data)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	return r.pool.Client(conn.Body.Settings)
}

// All returns every action this plugin exposes, in the order the canvas shows
// them: the two Cypher actions first, then the graph writes, the node read, and
// the schema action last.
func (r *Registry) All() []sdkv1.Action {
	return []sdkv1.Action{
		r.query(),
		r.execute(),
		r.nodeUpsert(),
		r.relationshipUpsert(),
		r.nodeFind(),
		r.constraintCreate(),
	}
}

// handler is what each action implements: pure work over a ready client, with
// finishing the job left to run. Returning an error fails the node.
type handler[T any] func(ctx context.Context, job *sdkv1.Job, client *neo4j.Client, in T) (map[string]any, error)

// run adapts a handler into an SDK job handler: decode the typed body and the
// connection that came with it, resolve the client, resolve {{$...}} tokens,
// report progress, and terminate the job exactly once on every path.
func run[T any](r *Registry, title string, fn handler[T]) sdkv1.JobHandler {
	return func(job sdkv1.Job) {
		req, err := sdkv1.CastRequestTo[T](job.Req.Data)
		if err != nil {
			job.DoneWithError("invalid request body: " + err.Error())
			return
		}
		client, err := r.clientFor(job.Req.Data)
		if err != nil {
			job.DoneWithError(err.Error())
			return
		}

		// Resolve {{$...}} tokens in every string input against the flow scope,
		// so the Cypher and each parameter can reference upstream data.
		resolveInputVars(&job, &req.Body)

		job.Progress(20, sdkv1.Frame{Title: title, Content: "on " + client.ConnInfo()})

		ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
		defer cancel()

		out, err := fn(ctx, &job, client, req.Body)
		if err != nil {
			job.DoneWithError(err.Error())
			return
		}
		if out == nil {
			out = map[string]any{}
		}

		job.Progress(90, sdkv1.Frame{Title: title, Content: "committing result"})
		job.Done(out)
	}
}
