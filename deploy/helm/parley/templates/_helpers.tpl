{{- define "parley.name" -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "parley.labels" -}}
app.kubernetes.io/part-of: parley
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* Selector labels for one component: include "parley.selector" (list . "api") */}}
{{- define "parley.selector" -}}
app.kubernetes.io/instance: {{ (index . 0).Release.Name }}
app.kubernetes.io/component: {{ index . 1 }}
{{- end -}}

{{/* Image for one of our components: include "parley.image" (list . "api") */}}
{{- define "parley.image" -}}
{{- $ctx := index . 0 -}}
{{- printf "%s/parley-%s:%s" $ctx.Values.image.registry (index . 1) (default $ctx.Chart.AppVersion $ctx.Values.image.tag) -}}
{{- end -}}

{{- define "parley.secretName" -}}
{{- default (printf "%s-secrets" (include "parley.name" .)) .Values.secrets.existingSecret -}}
{{- end -}}

{{/* Where the database URL lives: a secretKeyRef body. */}}
{{- define "parley.databaseUrlRef" -}}
{{- if and (not .Values.postgres.enabled) .Values.externalDatabase.existingSecret -}}
name: {{ .Values.externalDatabase.existingSecret }}
key: {{ .Values.externalDatabase.existingSecretKey }}
{{- else -}}
name: {{ include "parley.secretName" . }}
key: database-url
{{- end -}}
{{- end -}}

{{- define "parley.natsUrl" -}}
{{- if .Values.nats.enabled -}}
{{- printf "nats://%s-nats:4222" (include "parley.name" .) -}}
{{- else -}}
{{- required "externalNats.url is required when nats.enabled=false" .Values.externalNats.url -}}
{{- end -}}
{{- end -}}

{{- define "parley.httpScheme" -}}
{{- if .Values.ingress.tls.enabled }}https{{ else }}http{{ end -}}
{{- end -}}

{{- define "parley.wsScheme" -}}
{{- if .Values.ingress.tls.enabled }}wss{{ else }}ws{{ end -}}
{{- end -}}

{{- define "parley.podSecurity" -}}
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/* Our Go services (distroless "nonroot" = 65532; Kubernetes can only
     verify runAsNonRoot with a numeric user). */}}
{{- define "parley.containerSecurity" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}
