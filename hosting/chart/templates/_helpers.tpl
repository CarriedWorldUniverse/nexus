{{- define "hosted-service.name" -}}
{{- default .Chart.Name .Values.name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hosted-service.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end -}}

{{- define "hosted-service.labels" -}}
app.kubernetes.io/name: {{ include "hosted-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "hosted-service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hosted-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "hosted-service.identitySecretName" -}}
{{- default (printf "%s-keyfile" (include "hosted-service.name" .)) .Values.identity.secretName -}}
{{- end -}}
