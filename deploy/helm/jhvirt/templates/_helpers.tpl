{{- define "jhvirt.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "jhvirt.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "jhvirt.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "jhvirt.labels" -}}
helm.sh/chart: {{ include "jhvirt.chart" . }}
{{ include "jhvirt.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "jhvirt.selectorLabels" -}}
app.kubernetes.io/name: {{ include "jhvirt.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "jhvirt.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "jhvirt.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "jhvirt.validateValues" -}}
{{- $_ := required "externalURL is required, for example https://jhvirt.example.org" .Values.externalURL -}}
{{- $_ = required "database.existingSecret is required" .Values.database.existingSecret -}}
{{- $_ = required "appSecret.existingSecret is required" .Values.appSecret.existingSecret -}}
{{- if and (gt (int .Values.replicaCount) 1) (not (dig "cluster" "leader_election" false .Values.config)) }}
{{- fail "config.cluster.leader_election must be true when replicaCount is greater than 1" }}
{{- end }}
{{- if and (gt (int .Values.replicaCount) 1) .Values.backupStorage.enabled (not .Values.backupStorage.shared) }}
{{- fail "backupStorage.shared must be true for multiple replicas; use an RWX claim or an object/network storage target" }}
{{- end }}
{{- end }}

