INFO
====

Please see [go-redis/redis issue #1113](https://github.com/go-redis/redis/issues/1113) for details.

This repository contains a misbehaving Redis server and simple Go code to demonstrate the issue.

The Redis server isn't intended to be a real Redis server, but it does correctly respond to a few commands (`COMMAND`, `READONLY`, `ECHO`, `CLUSTER SLOTS`).
These are the only commands issued by this Go code.
Of course, since this isn't a real Redis server, it's not proof of the error, but you can replicate the same with a real Redis server, if you get (un?)lucky with timeouts.

The "misbehaving" part is that the first\* time it receives a `READONLY` command, it responds slowly.

(\*: it doesn't do this for the initial probing connection that goredis makes, to make the bug easier to understand).

To run:

```
    $ python fake-redis/server.py
```

Then, run
```
    $ go run main.go
```

You should see output similar to:
```
2019/07/23 17:23:26 msg: OK          
2019/07/23 17:23:26 err: <nil>
```

(note that if you rerun the Go code, you'll get a weird error about array parsing. This is how the same bug manifests when we timeout the first `READONLY` on the probing connection.)

Explanation
===========

For the purposes of code references, goredis/redis version [6bc7da](https://github.com/go-redis/redis/tree/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f) will be used, since this is the latest master at time of writing.

In short, this bug causes, in the best case, unexpected client-side errors and at worst, commands to read the replies from other commands.
Consider the case of a `ClusterClient` created with [`NewClusterClient`](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/cluster.go#L668).
The options passed to this `ClusterClient` must include `ReadOnly: true` (i.e. we're fine reading stale data to improve read throughput; see [redis docs](https://redis.io/commands/readonly) for more details) and `MaxRetries` at any value greater than zero.

Assume that the cluster state is known (i.e. the `CLUSTER SLOTS` command has been issued and a response has been returned) and no other connections have been made.
In my client code, I call `msg, err := client.Echo("message").Result()`.
The `ECHO` command replies whatever the argument given is ([ECHO documentation](https://redis.io/commands/echo)).

Details of how we get to the `READONLY` call (can be skipped if you're well acquainted with this and don't need a refresher):

- [commands.go:322](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/commands.go#L322): This creates a [`StringCmd`](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/command.go#L597) and passes it to `c`.
- [cluster.go:746](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/cluster.go#L746): A few levels of indirection later, the `StringCmd` is passed to `ClusterClient.process`. We assume that everything up until [L767](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/cluster.go#L767) succeeds. Since this is the first iteration, `ask` is `false` so we call `node.Client.ProcessContext(ctx, cmd)` on L755.
- [redis.go:244](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L244): Another few levels of indirection we arrive at `baseClient.process`. On L250 we call `c.getConn(ctx)`. `getConn` calls `_getConn`.
- [redis.go:167](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L167): `_getConn` requests a connection from the connection pool. Assume a clean, newly dialed connection is returned. We then call `c.initConn(cn)`.
- [redis.go:211](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L211): Since this is a new connection, the check at L206 is skipped. The options include `readOnly`, since `ReadOnly` was set on the cluster client and `ClusterOptions.clientOptions` which is passed to the `Client` from the cluster client, sets `readOnly` on [cluster.go:134](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/cluster.go#L134). Therefore, we pass this check.
- [redis.go:218](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L218): We call `newConn(c.opt, cn)`.
- [redis.go:635](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L635): `newConn` creates a `Conn` with its `connPool` containing a `NewSingleConnPool(cn)`.
- [redis.go:218](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L219): The function passed to `Pipelined` here contains only the `ReadOnly()` command based on the options.
- [redis.go:655](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L655): `Conn.Pipelined` passes its arguments to `Pipeline.Pipelined()` with `exec` as `c.processPipeline` (see [redis.go:659](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L659)).
- [pipeline.go:122](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/pipeline.go#L122): This is where the `ReadOnly` call is added to the pipeline, and `c.Exec` is called. Eventually that reaches the `processPipeline` we passed as `exec` earler.

**This is where the bug occurs**
- [redis.go:319](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L319): We call `generalProcessPipeline` with `pipelineProcessCmds` as `p`
- [redis.go:337](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L337): We call `getConn`.
- [redis.go:167](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L167): We call `connPool.Get()`. Since this is `SingleConnPool`, the pool's one `cn` is returned ([pool_single.go:25](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/internal/pool/pool_single.go#L25)). We call `initConn`, but the `cn` is already marked as `Inited`, so this is skipped and we return `cn`.
- [redis.go:343](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L343): We call `p(ctx, cn, cmds)`. `p` is `c.pipelineProcessCmds`.
- To recap: `c` is the `Conn`, `cn` is the SingleConnPool's connection, `cmds` is a list containing only `READONLY` (a `StatusCmd`).
- Assume that writing the response to the connection is fine and has no errors.
- Assume that the Redis server is under load and is slow to write the reply (`+OK`). This causes a timeout and `pipelineProcessCmds` returns `true, (timeout error)` at [redis.go:367](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L367).
- [redis.go:344](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L344): We call `c.releaseConnStrict(err)`.
- [redis.go:201](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L201): Since the error was not a Redis error, we call `c.connPool.Remove`.
- [pool_single.go:35](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/internal/pool/pool_single.go#L35) **`SingleConnPool.Remove` does nothing.**
- [redis.go:337](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L337): Second attempt (due to `MaxRetries`) We call `getConn`. We again get the connection from the `SingleConnPool` (the one used before).
- [redis.go:356](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L356): We succesfully write the `READONLY` command, then read the reply. By now, the response from the first `READONLY` has come to the connection, so it is read (since responses are read line-by-line, this only reads the first `+OK` response).

We now return with no error all the way back up the stack, but Redis has been sent a second `READONLY` command, which will reply with `+OK`, and be buffered in the connection.

- [redis.go:250](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L250): The connection is eventually returned here, with no error, but with the buffered `+OK` response.
- [redis.go:259](https://github.com/go-redis/redis/blob/6bc7daa5b1e86745a6976ac1c4dfe6c76ea6af1f/redis.go#L259): We sucessfully write our `ECHO message`. We then read the reply, which reads the `+OK` response. `StringCmd.readReply` calls `Reader.ReadString`, which can parse status replies as though they were strings (since they are).


At this point, the `ECHO` command returns `OK` instead of `message`, with no error.
The next call on the same connection will get a string response that was intended for the `ECHO` command.
This will continue, with no indication of an error in the client, until a command that expects a non-string, non-status response, at which point the reply will fail to read and the connection will be discarded.
