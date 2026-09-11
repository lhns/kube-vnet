{{/*
Standard Helm helpers.
*/}}

{{- define "kube-vnet.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-vnet.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kube-vnet.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-vnet.labels" -}}
helm.sh/chart: {{ include "kube-vnet.chart" . }}
{{ include "kube-vnet.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: controller
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "kube-vnet.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-vnet.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kube-vnet.serviceAccountName" -}}
{{ include "kube-vnet.fullname" . }}
{{- end -}}

{{- define "kube-vnet.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Selectors shared by both pod-resolution webhook configurations (ADR 0034).

They MUST be identical on the mutating and validating sides: a pod the
mutator skips but the validator judges would be checked against a resolution
that was never applied.

Namespaces excluded here mirror the operator's own --disabled-namespaces
default plus the release namespace. `kube-vnet/disabled=true` is an
ANNOTATION and so cannot be expressed as a namespaceSelector; the handlers
re-check it through the same NamespaceFilter the reconcilers use, which is
why that check is duplicated in Go.

The objectSelector keeps the operator's own pods out, so an operator restart
never depends on an operator that is not running yet.
*/}}
{{- define "kube-vnet.webhookSelectors" -}}
namespaceSelector:
  matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: NotIn
      values:
        - kube-system
        - kube-public
        - kube-node-lease
        - {{ .Release.Namespace }}
objectSelector:
  matchExpressions:
    - key: app.kubernetes.io/name
      operator: NotIn
      values:
        - {{ include "kube-vnet.name" . }}
{{- end }}
