"""ATS smoke for the end-to-end probe through the tunnel (giantswarm/tunnelport#110).

On gazelle a Teleport app service answered every new session with 504 for
thirteen minutes while the RemoteApps stayed Ready/Verified: nothing on the
consumer side sent a request *through* the tunnel. The operator now does, and
this smoke proves the whole reconciler path on the bare ATS kind cluster:

1. A RemoteApp whose upstream returns 504 becomes ``Ready=False`` with reason
   ``UpstreamUnreachable`` within one verification interval, the condition
   message carries the status and the probed URL, and a Warning Event is
   emitted.
2. When the upstream answers again — with a 401, which an OAuth resource
   server would — the RemoteApp recovers by itself and a Normal Event is
   emitted.

There is no Teleport here, so the smoke plays the tunnel itself. The operator
computes readiness from pods labelled ``tunnelport.giantswarm.io/role=tbot``
and ``tunnelport.giantswarm.io/remoteapp=<name>``, and its rendered Service
selects on the same labels, so a Pod carrying them stands in for the tunnel
end to end: it terminates TLS on the Service's ``tls`` port with a certificate
for the Service FQDN, signed by a CA the smoke mints and writes into the trust
bundle Secret named in tests/test-values.yaml (``verification.trustBundleSecretName``),
and answers the probe's ``GET /`` with the status under test. The operator's
own tbot Deployment for the RemoteApp also comes up and never becomes Ready
(there is no Teleport to join); that is fine, Ready is at-least-one-pod.
"""

import base64
import logging
import os
import subprocess
import tempfile
import time
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional

import pykube
import pytest
from pytest_helm_charts.clusters import Cluster

logger = logging.getLogger(__name__)

NAMESPACE = os.environ.get("ATS_RELEASE_NAMESPACE", "tunnelport-system")
MANAGER = "tunnelport"
# Must match verification.trustBundleSecretName in tests/test-values.yaml.
TRUST_BUNDLE_SECRET = "ats-smoke-trust-bundle"
TRUST_BUNDLE_KEY = "svid_bundle.pem"

REMOTEAPP = "probe-smoke"
TLS_PORT = 8443
FQDN = f"{REMOTEAPP}.{NAMESPACE}.svc.cluster.local"
PROBE_URL = f"https://{FQDN}:{TLS_PORT}/"
# gsoci mirror rather than Docker Hub: no anonymous pull limits from CI.
UPSTREAM_IMAGE = "gsoci.azurecr.io/giantswarm/nginx-unprivileged:1.31-alpine"
FIXTURE_LABEL = "ats.tunnelport.giantswarm.io/fixture"
TUNNEL_LABELS = {
    "tunnelport.giantswarm.io/role": "tbot",
    "tunnelport.giantswarm.io/remoteapp": REMOTEAPP,
    FIXTURE_LABEL: "upstream",
}
TLS_SECRET = f"{REMOTEAPP}-upstream-tls"

# Budgets. The first verdict waits on the kubelet propagating the trust-bundle
# Secret into the manager's optional mount (its own sync loop, about a minute,
# occasionally two) plus a 10s verification round; every later transition is
# one pod becoming Ready plus one round.
UPSTREAM_POD_TIMEOUT = 180
FIRST_VERDICT_TIMEOUT = 420
TRANSITION_TIMEOUT = 240
POLL = 5


def _run(*args: str) -> None:
    subprocess.run(list(args), check=True, capture_output=True)


def _mint_pki(workdir: Path) -> Dict[str, bytes]:
    """CA plus a server leaf for the Service FQDN, via the openssl CLI the ATS
    image ships (the pinned test dependencies carry no cryptography lib)."""
    ca_key, ca_crt = workdir / "ca.key", workdir / "ca.crt"
    leaf_key, leaf_csr, leaf_crt = workdir / "leaf.key", workdir / "leaf.csr", workdir / "leaf.crt"
    ext = workdir / "leaf.ext"
    ext.write_text(
        f"subjectAltName=DNS:{FQDN}\n"
        "extendedKeyUsage=serverAuth\n"
        "keyUsage=critical,digitalSignature\n"
        "basicConstraints=CA:FALSE\n"
    )
    _run("openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(ca_key))
    _run(
        "openssl", "req", "-x509", "-new", "-key", str(ca_key), "-sha256", "-days", "2",
        "-subj", "/CN=tunnelport-ats-smoke-ca", "-out", str(ca_crt),
    )
    _run("openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(leaf_key))
    _run("openssl", "req", "-new", "-key", str(leaf_key), "-subj", f"/CN={FQDN}", "-out", str(leaf_csr))
    _run(
        "openssl", "x509", "-req", "-in", str(leaf_csr), "-CA", str(ca_crt), "-CAkey", str(ca_key),
        "-CAcreateserial", "-days", "2", "-sha256", "-extfile", str(ext), "-out", str(leaf_crt),
    )
    return {"ca": ca_crt.read_bytes(), "crt": leaf_crt.read_bytes(), "key": leaf_key.read_bytes()}


def _b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


def _nginx_conf(status: int) -> str:
    # nginx-unprivileged includes conf.d/*.conf and runs as uid 101; 8443 is
    # an unprivileged port. `return` answers before any upstream lookup.
    return (
        "server {\n"
        f"  listen {TLS_PORT} ssl;\n"
        "  ssl_certificate /etc/upstream-tls/tls.crt;\n"
        "  ssl_certificate_key /etc/upstream-tls/tls.key;\n"
        f"  location / {{ return {status}; }}\n"
        "}\n"
    )


def _upstream_pod(name: str, status: int) -> Dict[str, Any]:
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": name, "namespace": NAMESPACE, "labels": dict(TUNNEL_LABELS)},
        "spec": {
            "restartPolicy": "Always",
            "containers": [
                {
                    "name": "upstream",
                    "image": UPSTREAM_IMAGE,
                    "ports": [{"name": "tls", "containerPort": TLS_PORT, "protocol": "TCP"}],
                    "readinessProbe": {"tcpSocket": {"port": TLS_PORT}, "periodSeconds": 2, "failureThreshold": 3},
                    "volumeMounts": [
                        {"name": "conf", "mountPath": "/etc/nginx/conf.d/default.conf", "subPath": "default.conf", "readOnly": True},
                        {"name": "tls", "mountPath": "/etc/upstream-tls", "readOnly": True},
                    ],
                }
            ],
            "volumes": [
                {"name": "conf", "configMap": {"name": f"{REMOTEAPP}-upstream-{status}"}},
                {"name": "tls", "secret": {"secretName": TLS_SECRET}},
            ],
        },
    }


def _conditions(ra: pykube.objects.APIObject) -> Dict[str, Dict[str, str]]:
    ra.reload()
    return {c["type"]: c for c in ra.obj.get("status", {}).get("conditions", [])}


def _wait(what: str, timeout: int, check: Callable[[], Optional[str]], on_timeout: Callable[[], str]) -> None:
    """Poll check() until it returns None (satisfied); its string is the
    reason it is not yet, kept for the failure message."""
    deadline = time.monotonic() + timeout
    reason = "not checked"
    while time.monotonic() < deadline:
        reason = check()
        if reason is None:
            logger.info("%s", what)
            return
        time.sleep(POLL)
    pytest.fail(f"timed out after {timeout}s waiting for {what}: {reason}\n{on_timeout()}")


def _manager_logs(api: pykube.HTTPClient) -> str:
    out: List[str] = []
    for pod in pykube.Pod.objects(api).filter(
        namespace=NAMESPACE, selector={"app.kubernetes.io/name": MANAGER, "tunnelport.giantswarm.io/role__notin": {"trust-bundle-bot"}}
    ):
        try:
            out.append(f"--- {pod.name} ---\n{pod.logs(tail_lines=60)}")
        except Exception as exc:  # noqa: BLE001 - diagnostics only
            out.append(f"--- {pod.name}: logs unavailable: {exc}")
    return "\n".join(out)


def _delete_quietly(obj: pykube.objects.APIObject) -> None:
    try:
        obj.delete()
    except pykube.exceptions.ObjectDoesNotExist:
        pass
    except Exception as exc:  # noqa: BLE001 - best-effort cleanup
        logger.warning("cleanup of %s/%s failed: %s", obj.kind, obj.name, exc)


@pytest.mark.smoke
def test_upstream_probe_folds_into_ready(kube_cluster: Cluster) -> None:
    api = kube_cluster.kube_client
    RemoteApp = pykube.object_factory(api, "access.giantswarm.io/v1alpha1", "RemoteApp")

    with tempfile.TemporaryDirectory() as tmp:
        pki = _mint_pki(Path(tmp))

    created: List[pykube.objects.APIObject] = []

    def create(obj: pykube.objects.APIObject) -> pykube.objects.APIObject:
        obj.create()
        created.append(obj)
        return obj

    def remoteapp() -> pykube.objects.APIObject:
        return RemoteApp.objects(api).filter(namespace=NAMESPACE).get(name=REMOTEAPP)

    def dump() -> str:
        try:
            status = remoteapp().obj.get("status")
        except Exception as exc:  # noqa: BLE001 - diagnostics only
            status = f"unavailable: {exc}"
        return f"RemoteApp status: {status}\nmanager logs:\n{_manager_logs(api)}"

    def upstream_pod_ready(name: str) -> Callable[[], Optional[str]]:
        def check() -> Optional[str]:
            pod = pykube.Pod.objects(api).filter(namespace=NAMESPACE).get(name=name)
            if pod.ready:
                return None
            return f"pod {name} phase={pod.obj.get('status', {}).get('phase')} statuses={pod.obj.get('status', {}).get('containerStatuses')}"

        return check

    def verdict(want_ready: bool, want_ready_reason: str, want_upstream: str, want_in_message: List[str]) -> Callable[[], Optional[str]]:
        def check() -> Optional[str]:
            ra = remoteapp()
            conds = _conditions(ra)
            ready, up = conds.get("Ready"), conds.get("UpstreamReachable")
            if ready is None or up is None:
                return f"conditions missing: {list(conds)}"
            if bool(ra.obj.get("status", {}).get("ready")) != want_ready or (ready["status"] == "True") != want_ready or ready["reason"] != want_ready_reason:
                return f"ready={ra.obj.get('status', {}).get('ready')} Ready={ready['status']}/{ready['reason']} ({ready.get('message')})"
            if up["status"] != want_upstream:
                return f"UpstreamReachable={up['status']}/{up['reason']} ({up.get('message')})"
            for want in want_in_message:
                if want not in up.get("message", ""):
                    return f"UpstreamReachable message {up.get('message')!r} lacks {want!r}"
            return None

        return check

    def event_seen(reason: str) -> Callable[[], Optional[str]]:
        def check() -> Optional[str]:
            events = list(
                pykube.Event.objects(api).filter(
                    namespace=NAMESPACE, field_selector={"involvedObject.name": REMOTEAPP, "reason": reason}
                )
            )
            if events:
                logger.info("Event %s: %s", reason, events[-1].obj.get("message") or events[-1].obj.get("note"))
                return None
            return f"no Event with reason {reason} for {REMOTEAPP}"

        return check

    try:
        # The bundle the manager verifies against; its optional mount fills in
        # once the kubelet notices the Secret.
        create(pykube.Secret(api, {
            "apiVersion": "v1", "kind": "Secret", "type": "Opaque",
            "metadata": {"name": TRUST_BUNDLE_SECRET, "namespace": NAMESPACE, "labels": {FIXTURE_LABEL: "trust-bundle"}},
            "data": {TRUST_BUNDLE_KEY: _b64(pki["ca"])},
        }))
        create(pykube.Secret(api, {
            "apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
            "metadata": {"name": TLS_SECRET, "namespace": NAMESPACE, "labels": {FIXTURE_LABEL: "upstream"}},
            "data": {"tls.crt": _b64(pki["crt"]), "tls.key": _b64(pki["key"])},
        }))
        for status in (504, 401):
            create(pykube.ConfigMap(api, {
                "apiVersion": "v1", "kind": "ConfigMap",
                "metadata": {"name": f"{REMOTEAPP}-upstream-{status}", "namespace": NAMESPACE, "labels": {FIXTURE_LABEL: "upstream"}},
                "data": {"default.conf": _nginx_conf(status)},
            }))
        create(RemoteApp(api, {
            "apiVersion": "access.giantswarm.io/v1alpha1", "kind": "RemoteApp",
            "metadata": {"name": REMOTEAPP, "namespace": NAMESPACE, "labels": {FIXTURE_LABEL: "upstream"}},
            "spec": {"appName": REMOTEAPP, "port": 8080, "tokenName": f"{REMOTEAPP}-token"},
        }))

        # 1. The incident: the tunnel is up, the far end answers 504.
        broken = create(pykube.Pod(api, _upstream_pod(f"{REMOTEAPP}-upstream-504", 504)))
        _wait(f"upstream pod {broken.name} Ready", UPSTREAM_POD_TIMEOUT, upstream_pod_ready(broken.name), dump)
        _wait(
            "Ready=False/UpstreamUnreachable with the 504 and the probed URL in the message",
            FIRST_VERDICT_TIMEOUT,
            verdict(False, "UpstreamUnreachable", "False", ["504", PROBE_URL]),
            dump,
        )
        conds = _conditions(remoteapp())
        assert conds["TunnelVerified"]["status"] == "True", f"TunnelVerified should hold while the upstream fails: {conds['TunnelVerified']}"
        assert remoteapp().obj["status"].get("lastError") == conds["UpstreamReachable"]["message"]
        _wait("a Warning Event with reason UpstreamUnreachable", TRANSITION_TIMEOUT, event_seen("UpstreamUnreachable"), dump)

        # 2. Recovery: swap the far end for one that answers 401 — reachable,
        #    as an OAuth resource server would be.
        _delete_quietly(broken)
        created.remove(broken)
        _wait(f"upstream pod {broken.name} gone", UPSTREAM_POD_TIMEOUT, lambda: None if not pykube.Pod.objects(api).filter(namespace=NAMESPACE, selector={FIXTURE_LABEL: "upstream"}).all() else "still terminating", dump)
        healthy = create(pykube.Pod(api, _upstream_pod(f"{REMOTEAPP}-upstream-401", 401)))
        _wait(f"upstream pod {healthy.name} Ready", UPSTREAM_POD_TIMEOUT, upstream_pod_ready(healthy.name), dump)
        _wait(
            "Ready=True with UpstreamReachable=True carrying the 401",
            TRANSITION_TIMEOUT,
            verdict(True, "TunnelReady", "True", ["401", PROBE_URL]),
            dump,
        )
        assert remoteapp().obj["status"].get("lastError", "") == ""
        _wait("a Normal Event with reason UpstreamReachable", TRANSITION_TIMEOUT, event_seen("UpstreamReachable"), dump)
    finally:
        for obj in reversed(created):
            _delete_quietly(obj)
