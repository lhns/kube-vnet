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
The username the apiserver reports for the operator's ServiceAccount. The
VAPs exempt it so the operator's own writes are admitted.
*/}}
{{- define "kube-vnet.operatorUsername" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "kube-vnet.serviceAccountName" .) -}}
{{- end -}}

{{/*
Non-empty when the cluster serves ValidatingAdmissionPolicy at
admissionregistration.k8s.io/v1 (GA since Kubernetes 1.30). Minor is
stripped of non-digits because some providers report e.g. "30+".
*/}}
{{- define "kube-vnet.vapSupported" -}}
{{- $major := int .Capabilities.KubeVersion.Major -}}
{{- $minor := int (regexReplaceAll "[^0-9]" .Capabilities.KubeVersion.Minor "") -}}
{{- if or (gt $major 1) (and (eq $major 1) (ge $minor 30)) }}true{{ end -}}
{{- end -}}

{{/*
Security context shared by the kubectl containers of the cleanup hook.
*/}}
{{- define "kube-vnet.cleanupSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
runAsUser: 65532
capabilities: { drop: [ALL] }
{{- end -}}

{{/*
Namespaces the operator leaves alone, as a JSON list. A null
operator.disabledNamespaces omits the flag, so the binary default applies;
mirror it here or templates that key on "is this namespace managed" disagree
with the operator.
*/}}
{{- define "kube-vnet.disabledNamespaces" -}}
{{- if kindIs "invalid" .Values.operator.disabledNamespaces -}}
{{- list "kube-system" | toJson -}}
{{- else -}}
{{- .Values.operator.disabledNamespaces | toJson -}}
{{- end -}}
{{- end -}}

{{/*
Namespaces the pod-resolution webhooks never see, as a JSON list: the
system namespaces, the release namespace and every disabled namespace, so
pod admission there never depends on the operator being up. Disabled
namespaces matter most: they typically hold the CNI or cert-manager, which
must be able to start while the operator is down. The system-labels VAP
polices pods in exactly these namespaces instead (see system-labels-vap.yaml).
*/}}
{{- define "kube-vnet.webhookExcludedNamespaces" -}}
{{- $ns := list "kube-system" "kube-public" "kube-node-lease" .Release.Namespace -}}
{{- $ns = concat $ns (include "kube-vnet.disabledNamespaces" . | fromJsonArray) -}}
{{- $ns | uniq | toJson -}}
{{- end -}}

{{/*
Selectors shared by both pod-resolution webhook configurations (ADR 0034).

They MUST be identical on the mutating and validating sides: a pod the
mutator skips but the validator judges would be checked against a resolution
that was never applied.

`kube-vnet/disabled=true` is an ANNOTATION and so cannot be expressed as a
namespaceSelector; the handlers re-check it through the same NamespaceFilter
the reconcilers use, which is why that check is duplicated in Go.

The objectSelector keeps the operator's own pods out, so an operator restart
never depends on an operator that is not running yet.

Whatever these selectors exclude, the system-labels VAP must cover; keep the
two in step.
*/}}
{{- define "kube-vnet.webhookSelectors" -}}
namespaceSelector:
  matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: NotIn
      values:
        {{- range include "kube-vnet.webhookExcludedNamespaces" . | fromJsonArray }}
        - {{ . }}
        {{- end }}
objectSelector:
  matchExpressions:
    - key: app.kubernetes.io/name
      operator: NotIn
      values:
        - {{ include "kube-vnet.name" . }}
{{- end }}
