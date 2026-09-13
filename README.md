# tracing-demo

Minimal end-to-end example: a Go program creates a `Foo` custom resource,
a controller reconciles it, and both sides show up as one connected trace
in Tempo — even though they never call each other directly, only exchange
state through the CRD.

## What's in here

```
cmd/creator/main.go       # creates a Foo, stashes trace context in annotations
cmd/controller/main.go    # watches Foo, extracts trace context, writes to /tmp
internal/tracing/         # shared OTel init/inject/extract helpers
Dockerfile.creator
Dockerfile.controller
build.sh                  # builds images, loads into kind/minikube/k3d
manifests/
  00-crd.yaml                    # the Foo CRD
  01-rbac.yaml                   # ServiceAccount + RBAC
  02-otel-collector-sidecar.yaml # OpenTelemetry Operator CR (sidecar mode)
  03-creator-job.yaml            # one-shot Job that creates a Foo
  04-controller-deployment.yaml  # long-running controller
  05-tempo.yaml                  # Tempo (monolithic, local storage)
  06-grafana.yaml                # Grafana pre-wired with a Tempo datasource
```

## Steps

1. **Install prerequisites** (once per cluster) — cert-manager and the
   OpenTelemetry Operator. See the comments at the top of
   `manifests/02-otel-collector-sidecar.yaml` for the exact commands.

2. **Apply the CRD, RBAC, and the sidecar CR:**
   ```bash
   kubectl apply -f manifests/00-crd.yaml
   kubectl apply -f manifests/01-rbac.yaml
   kubectl apply -f manifests/02-otel-collector-sidecar.yaml
   ```

3. **Deploy Tempo (and optionally Grafana):**
   ```bash
   kubectl apply -f manifests/05-tempo.yaml
   kubectl apply -f manifests/06-grafana.yaml   # optional but recommended
   ```

4. **Build and load the images:**
   ```bash
   chmod +x build.sh
   ./build.sh
   ```
   This runs `go mod tidy` (needs network access to the Go module proxy —
   run it locally, not in a sandboxed environment without internet), then
   builds both Docker images and loads them into whichever local cluster
   tool it detects (kind/minikube/k3d). If you're using a remote cluster,
   push the images to a registry instead and update the `image:` fields in
   `manifests/03-creator-job.yaml` and `manifests/04-controller-deployment.yaml`.

5. **Deploy the controller, then run the creator job:**
   ```bash
   kubectl apply -f manifests/04-controller-deployment.yaml
   kubectl apply -f manifests/03-creator-job.yaml
   ```

6. **Get the trace ID:**
   ```bash
   kubectl logs job/foo-creator | grep "trace id"
   ```

7. **View the trace:**
   - **Via Grafana**: port-forward and open Explore.
     ```bash
     kubectl port-forward svc/grafana 3000:3000
     ```
     Open http://localhost:3000, pick the **Tempo** datasource in Explore,
     and paste the trace ID.
   - **Via the Tempo API directly** (no Grafana needed):
     ```bash
     kubectl port-forward svc/tempo 3200:3200
     curl -s http://localhost:3200/api/traces/<TRACE_ID> | jq .
     ```

You should see one trace containing:
- `create-foo` (from `crd-creator`)
- `reconcile-foo` (from `foo-controller`), as a child span
  - `write-to-disk` (child of `reconcile-foo`)

## Checking the reconciled output

The controller writes `/tmp/foo-<namespace>-<name>.json` inside its own
container. Since the image is distroless (no shell), use an ephemeral
debug container to peek at it:

```bash
kubectl debug -it deploy/foo-controller --image=busybox --target=controller -- sh
# inside the debug shell:
cat /proc/1/root/tmp/foo-*.json
```

## Notes / things you may want to tune

- Tempo and its data volume here use `emptyDir`, so traces disappear if the
  pod restarts — fine for this experiment, not for real use.
- The sidecar pattern means each pod talks to `localhost:4317`. If you'd
  rather skip the sidecar entirely, point `OTEL_EXPORTER_OTLP_ENDPOINT` at
  `tempo.default.svc.cluster.local:4317` directly and drop the
  `sidecar.opentelemetry.io/inject` annotations — one less moving part,
  same result, just no per-pod batching/isolation.
- `go.sum` isn't included — `build.sh` generates it via `go mod tidy`,
  which needs network access to the Go module proxy.
