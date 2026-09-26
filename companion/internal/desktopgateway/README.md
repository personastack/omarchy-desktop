# desktopgateway

`desktopgateway` owns the outbound, authenticated Desktop Control websocket
between the Linux companion and the API-owned `agentgatewayruntime` protocol.
It bounds command concurrency, checks producer-owned frame contracts and
deadlines, reports readiness with heartbeats, and never retries commands. The
read loop stays bound to the socket generation that started it. Serialized
websocket writes have a bounded deadline (15 seconds by default, 30 seconds
maximum), reduced to the command deadline for command results and chunks. Late
output is dropped without closing a healthy connection.

The command handler is supplied by the local Cua and executor runtime. This
package does not claim GUI, file, or process readiness by itself.
