{{/* vim: set filetype=mustache: */}}

{{/*
Chart name (always "tunnelport"; truncated for safety).
*/}}
{{- define "tunnelport.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified resource name. Stem for ServiceAccount / Deployment /
ClusterRole etc. Operator-only resources live in `installNamespace`;
the rendered tbot resources live in the RemoteApp's namespace and are
named after the CR — those are out of this chart's hands.
*/}}
{{- define "tunnelport.fullname" -}}
{{- printf "%s" (include "tunnelport.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Chart label (helm.sh/chart). Sanitised per Helm convention.
*/}}
{{- define "tunnelport.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels stamped on every operator-owned object the chart renders.
These describe the *operator*; rendered tbot pods get a different label
set (see CONTEXT.md / README.md "Pod labels for NetworkPolicy"), which
is the operator code's responsibility, not the chart's.
*/}}
{{- define "tunnelport.labels" -}}
helm.sh/chart: {{ include "tunnelport.chart" . }}
{{ include "tunnelport.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: {{ index .Chart.Annotations "application.giantswarm.io/team" | quote }}
{{- end -}}

{{/*
Selector labels — must be a strict subset of common labels, and must
match between Deployment.spec.selector and the manager pod template.
*/}}
{{- define "tunnelport.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tunnelport.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Manager image reference. `image.tag` falls back to .Chart.Version, which
app-build-suite stamps from the git tag at release time and which matches
the image tag the pipeline pushes. (.Chart.Version is the single source of
truth for the tag; appVersion is also stamped by the release orb but is not
relied on here.) Build metadata (the "+<digest>" Flux appends when pulling
the chart by OCI digest) is stripped, since image tags cannot contain "+"
and the pushed tag is the clean semver.
*/}}
{{- define "tunnelport.image" -}}
{{- $tag := default (.Chart.Version | splitList "+" | first) .Values.image.tag -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.name $tag -}}
{{- end -}}

{{/*
tbot image reference. Composed from registry/name/tag so the registry can
be overridden (e.g. to pull from a non-gsoci mirror) while keeping the
`restrict-image-registries` Kyverno policy satisfied by default. An optional
`digest` pins the image by content (`<repo>:<tag>@sha256:<hex>`).
*/}}
{{- define "tunnelport.tbotImage" -}}
{{- $img := .Values.tbot.image -}}
{{- $ref := printf "%s/%s:%s" $img.registry $img.name $img.tag -}}
{{- if $img.digest }}{{- $ref = printf "%s@%s" $ref $img.digest -}}{{- end -}}
{{- $ref -}}
{{- end -}}

{{/*
ghostunnel sidecar image reference. Same registry/name/tag/digest shape as
tunnelport.tbotImage.
*/}}
{{- define "tunnelport.ghostunnelImage" -}}
{{- $img := .Values.tls.image -}}
{{- $ref := printf "%s/%s:%s" $img.registry $img.name $img.tag -}}
{{- if $img.digest }}{{- $ref = printf "%s@%s" $ref $img.digest -}}{{- end -}}
{{- $ref -}}
{{- end -}}
