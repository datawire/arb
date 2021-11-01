# ARB

Emissary-ingress and Ambassador Edge Stack both support the Envoy Access Log Service (ALS)
for handing access logs to an external service (the ALS). The ALS speaks a gRPC protocol
with Envoy; the **A**ccess Log Service **R**EST **B**ridge (ARB) provides a bridge to allow
REST services to use the access logs too.

ARB is a separate service, deployed independently of Emissary-ingress or Ambassador Edge
Stack. Multiple ARBs can be deployed if desired. As log entries arrive from Envoy, they
are batched within ARB, then dispatched in parallel to one or more upstream REST services.
If a REST call fails with a 5YZ status, ARB will retry, with a configurable backoff between
retries, up to a configurable maximum number of retries. For other errors, ARB will *not*
retry, though it will log the error.

Normally, ARB uses an internal circular buffer with a fixed maximum size to batch requests:
if the upstream services are not able to keep up with the rate of incoming messages, ARB
will drop the oldest messages in the buffer. If messages are dropped, ARB will log a warning
every five minutes to that effect. The size of this circular buffer is configurable,
and if the size is set to 0, the queue will simply grow without bound (which risks ARB running
out of memory and crashing).

The REST requests are simple POSTS to whatever URLs are configured for the upstream
services, with `Content-Type: application/json` and a body that is a JSON-encoded array
of Envoy [`HTTPAccessLogEntries`]. The upstream services can handle the entries in 
whatever way is desired: ARB's only requirement is that a 200 response be returned so 
that ARB knows it need not retry the request.

At present, ARB uses V2 `HTTPAccessLogEntries`, since that is still the default for
Envoy's `AccessLogService`. A future version of ARB will support both V2 and V3.

An example POST body might look like

```
[
    {
        "commonProperties":{
            "downstreamDirectRemoteAddress":{
                "socketAddress":{
                    "address":"10.42.0.24",
                    "portValue":42402
                }
            },
            "downstreamLocalAddress":{
                "socketAddress":{
                    "address":"10.42.0.15",
                    "portValue":8080
                }
            },
            "downstreamRemoteAddress":{
                "socketAddress":{
                    "address":"10.42.0.24",
                    "portValue":42402
                }
            },
            "startTime":"2021-10-27T21:42:00.378628600Z",
            "timeToFirstDownstreamTxByte":"0.001517100s",
            "timeToFirstUpstreamRxByte":"0.001448800s",
            "timeToFirstUpstreamTxByte":"0.000273400s",
            "timeToLastDownstreamTxByte":"0.001586900s",
            "timeToLastRxByte":"0.000119400s",
            "timeToLastUpstreamRxByte":"0.001540100s",
            "timeToLastUpstreamTxByte":"0.000297s",
            "upstreamCluster":"cluster_quote_default_default",
            "upstreamLocalAddress":{
                "socketAddress":{
                    "address":"10.42.0.15",
                    "portValue":42174
                }
            },
            "upstreamRemoteAddress":{
                "socketAddress":{
                    "address":"10.43.18.95",
                    "portValue":80
                }
            }
        },
        "protocolVersion":"HTTP11",
        "request":{
            "authority":"emissary-ingress.emissary",
            "forwardedFor":"10.42.0.24",
            "originalPath":"/backend/",
            "path":"/request-path/",
            "requestHeadersBytes":"270",
            "requestId":"a9241926-9759-4460-ba5e-bab06c5e93d1",
            "requestMethod":"GET",
            "scheme":"http",
            "userAgent":"curl/7.64.1"
        },
        "response":{
            "responseBodyBytes":"142",
            "responseCode":200,
            "responseCodeDetails":"via_upstream",
            "responseHeadersBytes":"129"
        }
    }
]
```

For details on the format of an `HTTPAccessLogEntry`, see the [`HTTPAccessLogEntry` specification].

[`HTTPAccessLogEntries`]: https://www.envoyproxy.io/docs/envoy/v1.17.4/api-v2/data/accesslog/v2/accesslog.proto#envoy-api-file-envoy-data-accesslog-v2-accesslog-proto
[`HTTPAccessLogEntry` specification]: https://www.envoyproxy.io/docs/envoy/v1.17.4/api-v2/data/accesslog/v2/accesslog.proto#envoy-api-file-envoy-data-accesslog-v2-accesslog-proto

## Configuration

The ARB reads its configuration from a Kubernetes ConfigMap. By default, this map is named
`arb-configuration`, and the ARB mounts it from the namespace in which the ARB is installed.
If need be, you can edit the ARB Deployment to mount a different ConfigMap.

**Note well**: You will need to create the ARB ConfigMap before deploying ARB. Since the configuration is mounted into the ARB Pods, deployment will fail if the ConfigMap does not exist. Also note that ARB 1.0.0 must be restarted when its configuration is changed. 

The contents of the map are:

| Parameter         | Type              | Default | Semantics |
|-------------------|-------------------|---------|-----------|
| `port`            | `int`             | 9001    | Port on which to listen |
| `queueSize`       | `int`             | 4096    | Maximum number of messages to buffer; 0 to grow indefinitely |
| `batchSize`       | `int`             | 5       | Number of messages to receive from Envoy before sending to REST services |
| `batchDelay`      | `duration`        | 30s     | Interval between batches |
| `requestTimeout`  | `duration`        | 300ms   | Request timeout for REST calls |
| `retries`         | `int`             | 3       | Number of times to retry |
| `retryDelay`      | `duration`        | 30s     | Initial delay before retry |
| `retryMultiplier` | `int`             | 2       | Multiplier for retry delay on each retry |
| `services`        | array of `string` | None    | Services to send REST requests |

A sample configuration might be:

```yaml
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: arb-configuration
data:
  requestTimeout: "500ms"
  retries: "5"
  services: |-
    https://foo.example.com/service1
    https://bar.example.com/service2
    http://cleartext.example.com/do-not-use
```

In this example, ARB will listen on port 9001. REST requests will be sent whenever Envoy
has sent five updates, or if Envoy has sent at least one update and 30 seconds have passed
since the last REST requests were sent. REST requests will be sent to each of the three
services listed, in parallel. A given REST request will have 500ms to complete. A timeout
or `5YZ` response will be retried up to 5 times, waiting 30s between the failed initial
request and the first retry, with the delay doubling on each retry (so the maximum delay -
between retries 4 and 5 - will be 8 minutes).

Note that ARB retries _`5YZ`_ retries, not _`4YZ`_ responses. A `4YZ` response indicates
that something about the request is wrong: it is unlikely to succeed if retried. A `5YZ`
response indicates that something has gone wrong in the server's processing of the request:
it is at least possible that a retry will succeed. However, there are three `5YZ` codes
that will not be retried:

* `501 Not Implemented`
* `505 HTTP Version Not Supported`
* `511 Network Authentication Required`

These three responses are likely to indicate situations that will not spontaneously resolve
(for example, if the server does not support the version of HTTP that ARB is using, that is
unlikely to be corrected in the next few minutes), so ARB will not retry them.

If a request fails after all retries, ARB will log a failure message:

```
FAILED: $request got $status on final retry
```

For example:

```
FAILED: http://foo.example.com/service1 got 503 on final retry
```

An example ARB deployment might look like:

```yaml
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: arb
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: arb
  strategy:
    rollingUpdate:
      maxSurge: 25%
      maxUnavailable: 25%
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/name: arb
    spec:
      containers:
      - image: docker.io/datawire/arb:1.0.0
        imagePullPolicy: IfNotPresent
        name: arb
        terminationMessagePath: /dev/termination-log
        terminationMessagePolicy: File
        volumeMounts:
        - name: config-volume
          mountPath: /etc/arb-config
              restartPolicy: Always
      serviceAccount: arb-account
      serviceAccountName: arb-account
      terminationGracePeriodSeconds: 30
      volumes:
      - name: config-volume
        configMap:
          name: arb-configuration
```

**Note well**: You will need to create the ARB ConfigMap before deploying ARB. Since the
configuration is mounted into the ARB Pods, deployment will fail if the ConfigMap does
not exist. Also note that ARB 1.0.0 must be restarted when its configuration is changed.

Once deployed, a `LogService` must be configured to tell Edge Stack to send
logs to the ARB:

```
---
apiVersion: getambassador.io/v3alpha1
kind: LogService
metadata:
  name: arb-log-service
spec:
  service: arb
  driver: http
  grpc: true
```

Once the `LogService` is added, the ARB will receive logs from Envoy and send
them on to the various REST services as configured.

**Note well**: If you don't create the `LogService`, no log traffic will be sent to the
ARB, and it will not have any useful effect. In this case, ARB will log a warning every
hour:

```
WARNING: no requests in one hour; check your LogService configuration
```

## Running the demo

Set up a cluster and install version 2.0.4 or higher of either Emissary or Edge Stack.
(Edge Stack is preferred, since its built-in rate limiter provides metadata that the
`arblogger` demo REST service can use.)

AFTER installing Emissary or Edge Stack, run `make demo` to install other demo resources.
This will install:

- A `Listener` and `Host` to allow HTTP routing
- ARB itself (using `ko`; see below)
- Three instances of the demo `arblogger` REST service (source is available at 
  https://github.com/datawire/arblogger)
- A `LogService` which feeds data to the ARB
- An ARB configuration which feeds data to the `arblogger` services
- The Quote of the Moment service
- A `Mapping` from `/backend/` to the Quote of the Moment service
- A `Mapping` from `/foo` to `https://httpbin.org`
- A `Mapping` from `/bar` to `https://httpbin.org`
- If Edge Stack is installed, a `RateLimit` resource that applies rate limits
  to the `/foo` and `/bar` `Mapping`s

Once this is all installed, you can watch the logs for the `arblogger` and `arb`
instances, and send requests using the IP of your Emissary or Edge Stack service:

- `http://$IP/backend/` will never be rate limited

- If Edge Stack is installed:
  - `http://$IP/foo/ip` will allow up to 3 requests per minute (bursting up to 15), 12 per hour, 100 per day
  - `http://$IP/bar/ip` will allow up the 3 requests per minute

If Emissary is installed, the `foo` and `bar` rate limits will not apply.

The demo ARB configuration has a batch size of 10, a batch delay of 30s, and a queue size of
30, so:

- requests may take 30 seconds to be sent upstream, unless you send 10 or more in quick
  succession, and
- only 30 requests can be in the queue at a time.

Note that Envoy can also take up to 10 seconds to pass a request to ARB.

Requests may arrive out of order, since the demo configuration has multiple instances of
Emissary or Edge Stack. Since the demo `arblogger` will log the `X-Request-ID` header,
supplying unique `X-Request-ID` headers can be helpful for seeing exactly what's going on:
(If no `X-Request-ID` is in your request, Envoy will supply a UUID for it.)

The demo configuration uses the three demo `arblogger` instances differently:

- It uses HTTPS to `arblogger1`, which is configured to always return 200 when ARB
  contacts it. You will see output from this `arblogger`, but you shouldn't see ARB
  itself logging much about it.
- It also uses HTTPS to `arblogger2`, which is configured to always return 404 when
  ARB contacts it. You will see output from this `arblogger`, but you should also see
  ARB logging `FAILED: https://arblogger2/404 got 404 on final retry`.
- Finally, it uses HTTP to `arblogger3`, which is configured to always return 503 when
  ARB contacts it. You will see output from this `arblogger`, but ARB will constantly
  be retrying requests to it, so you will see `FAILED: http://arblogger3/503 got 503 on
  final retry` messages from ARB (eventually), and you will also see ARB logging about
  `Mgr 2` dropping entries.

(If you send requests more quickly than `arblogger1` and `arblogger2` can process them,
you will eventually see messages about `Mgr 0` and `Mgr 1` dropping entries. Each upstream
has an `Mgr` and a `Wrk` goroutine; the `Mgr` goroutine manages the queue for that upstream,
and the `Wrk` goroutine actually makes the REST requests.)

## Debugging ARB

Set the environment variable `ARB_LOG_LEVEL=debug` to enable more debug logging from
ARB.

## Building ARB

### Install dependencies

 - [GNU Make](https://gnu.org/s/make)
 - [Go](https://golang.org/) 1.15 or newer
 - Docker

### Set up

Edit the `Makefile` to set `DOCKER_REGISTRY` to a registry to which 
you can push. You may also prefer to set `IMAGE_TAG` to give your image
a separate version number.

After that, `make tools` to set up [`ko`](https://github.com/google/ko)
in `tools/bin/ko`.

### Using `ko` for development

`make apply` will use `ko` to build ARB and apply it, using `arb.yaml`,
to your cluster.

### Pushing an image to your Docker registry

`make push` will build ARB with `ko`, then push it to `$DOCKER_REGISTRY/arb:$IMAGE_TAG`,
where the variables have their values from the `Makefile`.
