# No `pods/log` RBAC; status surfaces k8s-visible state only

The operator does not request the `pods/log` RBAC verb and never reads tbot
container logs. `RemoteApp.status.lastError` reflects k8s-visible state
only — Pod `Phase`, `ContainerStatuses`, `RestartCount`, last termination
reason — and contains entries like
`"CrashLoopBackOff (5 restarts), last termination: Error (137)"`. To see why
a tbot is failing, the platform engineer runs `kubectl logs` themselves.

Considered and rejected: tailing the failing container's last log line into
`status.lastError`, or going further and classifying errors
(`ProxyReachable=false`, `TokenAccepted=false`, etc.) by parsing tbot logs.
Both couple the operator to tbot's log format, which isn't a stable API and
silently breaks across tbot version bumps. Once `pods/log` is granted,
future contributors will be tempted to add log-based decision-making
("smart retry", "auto-recovery"), which compounds the coupling.

This ADR exists primarily to prevent that drift: pin the rule that the
operator never reads or parses tbot logs, in code or in status. If
diagnosis ergonomics become a real complaint, scrape tbot's Prometheus
metrics — that's a stable interface tbot ships.
