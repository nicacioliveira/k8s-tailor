{{/*
Everything namespaced is rendered into .Release.Namespace (`helm install -n <ns>`
/ ArgoCD destination.namespace). The legacy `namespace` value is only accepted
when it matches, so an old values file can't silently split the release.
*/}}
{{- define "tailor.namespace" -}}
{{- if and .Values.namespace (ne .Values.namespace .Release.Namespace) -}}
{{- fail (printf "values.namespace=%q no longer moves resources: install with `-n %s` (helm) / destination.namespace (ArgoCD) and drop `namespace` from your values (release namespace is %q)" .Values.namespace .Values.namespace .Release.Namespace) -}}
{{- end -}}
{{- .Release.Namespace -}}
{{- end -}}

{{/* Common labels for chart-owned objects. */}}
{{- define "tailor.labels" -}}
app.kubernetes.io/name: k8s-tailor
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
tailor.image — image reference from either a string ("repo:tag", back-compat with
`--set tailor.image=k8s-tailor:dev`) or a map {repository, tag, digest}. The tag
defaults to v<chart appVersion> (the git tag CI publishes); a digest pins it immutably.
Usage: include "tailor.image" (dict "img" .Values.tailor.image "ctx" $)
*/}}
{{- define "tailor.image" -}}
{{- $img := .img -}}
{{- if kindIs "string" $img -}}
{{- $img -}}
{{- else -}}
{{- $repo := required "image.repository is required" $img.repository -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repo $img.digest -}}
{{- else -}}
{{- /* CI (build-images.yml, type=ref,event=tag) publishes git tags: v<appVersion>. */ -}}
{{- $app := .ctx.Chart.AppVersion -}}
{{- $def := ternary $app (printf "v%s" $app) (hasPrefix "v" $app) -}}
{{- printf "%s:%s" $repo (toString ($img.tag | default $def)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
tailor.imagePullSecrets — pod-spec imagePullSecrets from global.imagePullSecrets
(the same key the VictoriaMetrics subchart reads, so one setting covers every
pod). Items are {name: <secret>} (the subchart needs that form).
*/}}
{{- define "tailor.imagePullSecrets" -}}
{{- $l := list -}}
{{- range ((.Values.global | default dict).imagePullSecrets | default list) -}}
{{- if kindIs "string" . -}}{{- $l = append $l (dict "name" .) -}}{{- else -}}{{- $l = append $l . -}}{{- end -}}
{{- end -}}
{{- with $l -}}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end -}}
{{- end -}}

{{/*
tailor.lookupAvailable — "true" when the chart is rendered against a live API
server (helm install/upgrade, --dry-run=server). Empty under `helm template`,
client --dry-run, `helm lint` and ArgoCD/Flux renders: there `lookup` returns
nothing, so a generated secret can't be reused and would change on every render.
kube-system exists in every cluster, so an empty answer means "no API server".
*/}}
{{- define "tailor.lookupAvailable" -}}
{{- if lookup "v1" "Namespace" "" "kube-system" -}}true{{- end -}}
{{- end -}}

{{/*
tailor.secretValue — value for a chart-managed secret key, by precedence:
  1. .value (explicitly configured in values)
  2. the value already stored in the live Secret (Helm `lookup`) — so a generated
     secret is stable across `helm upgrade`
  3. a fresh random 32-char string (first install).
Without lookup (helm template / ArgoCD / Flux) (3) would regenerate on every
render: with secretGeneration.failWithoutLookup=true the render FAILS instead —
set an existingSecret (or the inline value) for GitOps.
Usage: include "tailor.secretValue" (dict "ns" $ns "name" "x" "key" "token" "value" .Values.x "what" "tailor.apiAuth" "ctx" $)
*/}}
{{- define "tailor.secretValue" -}}
{{- if .value -}}
{{- .value -}}
{{- else -}}
{{- $s := lookup "v1" "Secret" .ns .name -}}
{{- if and $s $s.data (hasKey $s.data .key) -}}
{{- index $s.data .key | b64dec -}}
{{- else -}}
{{- if and (not (include "tailor.lookupAvailable" .ctx)) .ctx.Values.secretGeneration.failWithoutLookup -}}
{{- fail (printf "%s: no Kubernetes API available to reuse the generated secret %s/%s (helm template / ArgoCD / Flux render); a fresh random value would be minted on EVERY render. Set %s.existingSecret (e.g. an ExternalSecret) or an explicit value — see docs/UPGRADE.md#gitops" .what .ns .name .what) -}}
{{- end -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Name of the Secret holding the ClickHouse password. */}}
{{- define "tailor.chSecretName" -}}
{{- .Values.clickhouse.auth.existingSecret | default "k8s-tailor-clickhouse-auth" -}}
{{- end -}}

{{/* Whether the engine reads CH_PASSWORD from a Secret (vs. tailor.env inline). */}}
{{- define "tailor.chPasswordFromSecret" -}}
{{- if or .Values.clickhouse.auth.existingSecret .Values.clickhouse.enabled -}}true{{- end -}}
{{- end -}}

{{/* Engine write-API token Secret (apiAuth.enabled). */}}
{{- define "tailor.apiSecretName" -}}
{{- .Values.tailor.apiAuth.existingSecret | default "k8s-tailor-api-auth" -}}
{{- end -}}

{{/* MCP Bearer Secret. */}}
{{- define "tailor.mcpSecretName" -}}
{{- .Values.mcp.auth.existingSecret | default "k8s-tailor-mcp-auth" -}}
{{- end -}}

{{/* Name of the webhook serving-cert Secret. */}}
{{- define "tailor.webhookSecretName" -}}
{{- if eq (.Values.tailor.webhook.certMode | default "helm") "existingSecret" -}}
{{- required "tailor.webhook.tls.existingSecret is required with certMode=existingSecret" .Values.tailor.webhook.tls.existingSecret -}}
{{- else -}}
k8s-tailor-webhook-tls
{{- end -}}
{{- end -}}

{{/*
tailor.excludeNamespaces — comma list for EXCLUDE_NAMESPACES: the release
namespace (the Tailor never sizes itself, its ClickHouse or vmsingle) +
tailor.excludeNamespaces + any legacy tailor.env.EXCLUDE_NAMESPACES.
*/}}
{{- define "tailor.excludeNamespaces" -}}
{{- $l := list (include "tailor.namespace" .) -}}
{{- range .Values.tailor.excludeNamespaces -}}{{- $l = append $l (trim .) -}}{{- end -}}
{{- with .Values.tailor.env.EXCLUDE_NAMESPACES -}}
{{- range splitList "," . -}}{{- if trim . -}}{{- $l = append $l (trim .) -}}{{- end -}}{{- end -}}
{{- end -}}
{{- $l | uniq | join "," -}}
{{- end -}}

{{/* CRD resource-model schema (cpu / memory) shared by TailorPolicy, TailorProfile and container rules. Bounds mirror internal/policy/validate.go. */}}
{{- define "tailor.crd.resSpec" -}}
type: object
properties:
  quantile:
    type: number
    {{- with .quantile }}
    default: {{ . }}
    {{- end }}
    minimum: 0
    exclusiveMinimum: true
    maximum: 1
    description: Usage quantile the request is sized from (0 < q <= 1).
  margin:
    type: number
    {{- with .margin }}
    default: {{ . }}
    {{- end }}
    minimum: 0
    maximum: 10
    description: Headroom fraction above the quantile (0.15 = +15%).
  min:
    type: string
    pattern: '^[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$'
    description: Floor quantity (e.g. 10m, 32Mi).
  max:
    type: string
    pattern: '^[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$'
    description: Ceiling quantity (e.g. 4, 16Gi).
  limitFactor:
    type: number
    {{- if hasKey . "limitFactor" }}
    default: {{ .limitFactor }}
    {{- end }}
    minimum: 0
    maximum: 100
    x-kubernetes-validations:
      - rule: "self == 0.0 || self >= 1.0"
        message: "limitFactor must be 0 (unset / no limit) or >= 1 (a limit below the request is invalid)"
    description: limit = request x factor. 0 = unset (no CPU limit).
{{- end -}}

{{/* Go/Tailor duration: 30s, 1h30m, 3d, 2w. */}}
{{- define "tailor.crd.durationPattern" -}}
'^([0-9]+(\.[0-9]+)?[dw]|([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+)$'
{{- end -}}

{{/* Metadata for chart-managed CRDs (kept on uninstall unless crds.keep=false). */}}
{{- define "tailor.crd.meta" -}}
labels:
  {{- include "tailor.labels" . | nindent 2 }}
{{- if .Values.crds.keep }}
annotations:
  # Deleting a CRD deletes every TailorPolicy/Profile/Group/Config with it.
  helm.sh/resource-policy: keep
{{- end }}
{{- end -}}
