{{/*
Expand the name of the chart.
*/}}
{{- define "dex-idp-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "dex-idp-operator.fullname" -}}
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

{{/*
Namespace for generated references.
Always uses the Helm release namespace.
*/}}
{{- define "dex-idp-operator.namespaceName" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
Resource name with proper truncation for Kubernetes 63-character limit.
Takes a dict with:
  - .suffix: Resource name suffix (e.g., "metrics", "webhook")
  - .context: Template context (root context with .Values, .Release, etc.)
Dynamically calculates safe truncation to ensure total name length <= 63 chars.
*/}}
{{- define "dex-idp-operator.resourceName" -}}
{{- $fullname := include "dex-idp-operator.fullname" .context }}
{{- $suffix := .suffix }}
{{- $maxLen := sub 62 (len $suffix) | int }}
{{- if gt (len $fullname) $maxLen }}
{{- printf "%s-%s" (trunc $maxLen $fullname | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" $fullname $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name to use.
If serviceAccount.enabled is false and serviceAccount.name is set, use that name.
Otherwise, use the standard resourceName helper with "controller-manager" suffix.
*/}}
{{- define "dex-idp-operator.serviceAccountName" -}}
{{- if and (not (.Values.serviceAccount.enabled | default true)) .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "dex-idp-operator.resourceName" (dict "suffix" "controller-manager" "context" .) }}
{{- end }}
{{- end }}

{{/*
Name of the optional in-chart Dex Deployment/Service (dex.enabled=true).
*/}}
{{- define "dex-idp-operator.dexName" -}}
{{- include "dex-idp-operator.resourceName" (dict "suffix" "dex" "context" .) }}
{{- end }}

{{/*
Name of the DexConnection resource created for the optional in-chart Dex Deployment.
Defaults to the Dex Deployment/Service name when dex.dexConnection.name is unset.
*/}}
{{- define "dex-idp-operator.dexConnectionName" -}}
{{- if .Values.dex.dexConnection.name }}
{{- .Values.dex.dexConnection.name }}
{{- else }}
{{- include "dex-idp-operator.dexName" . }}
{{- end }}
{{- end }}
