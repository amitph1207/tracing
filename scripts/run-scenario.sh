#!/usr/bin/env bash
# Create a uniquely named Job and Foo; no deletion is needed to repeat an experiment.
set -euo pipefail
cd "$(dirname "$0")/.."
scenario="${1:-chain}"
case "$scenario" in
  chain) manifest=manifests/03-creator-job.yaml ;;
  retry|linked|missing|invalid|unsampled|timeout|slow|update) manifest="manifests/scenarios/$scenario.yaml" ;;
  *) echo "Usage: $0 {chain|retry|linked|missing|invalid|unsampled|timeout|slow|update} [existing-foo-for-update]" >&2; exit 2 ;;
esac
if [[ "$scenario" == update && -z "${2:-}" ]]; then
  echo "Usage: $0 update <existing-foo-name>" >&2
  exit 2
fi
kubectl create --dry-run=client -f "$manifest" -o json |
  python3 -c '
import json, sys, uuid
obj = json.load(sys.stdin)
scenario, existing = sys.argv[1:]
name = "foo-" + scenario + "-" + uuid.uuid4().hex[:10]
obj["metadata"]["name"] = name
obj["spec"]["backoffLimit"] = 0
container = obj["spec"]["template"]["spec"]["containers"][0]
for env in container["env"]:
    if env["name"] == "FOO_NAME":
        env["value"] = existing if scenario == "update" else name
    if env["name"] == "MESSAGE":
        env["value"] = "new desired state " + name
print(json.dumps(obj))
' "$scenario" "${2:-}" |
  kubectl create -f -
echo 'Watch the new Job with: kubectl logs -f job/<printed-job-name>'
echo 'Inspect the chain with: kubectl get foos.example.com,bars.example.com,bazs.example.com'
echo 'Read child annotations/status with: kubectl get bars.example.com <child-name> -o yaml'
