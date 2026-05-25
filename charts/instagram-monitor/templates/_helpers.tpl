{{- /*
Expand the name of the chart.
*/ -}}
{{- define "instagram-monitor.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- /*
A label contains the chart name and version.
*/ -}}
{{- define "instagram-monitor.labels" -}}
helm.sh/chart: {{ include "instagram-monitor.chart" . }}
{{ include "instagram-monitor.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- /*
Selector labels
*/ -}}
{{- define "instagram-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "instagram-monitor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- /*
Create chart name and version as used by the chart label.
*/ -}}
{{- define "instagram-monitor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- /*
Render a JSON string array from a list of strings.
Usage: {{ include "instagram-monitor.toJsonArray" .Values.monitors }}
*/ -}}
{{- define "instagram-monitor.toJsonArray" -}}
{{- $items := . -}}
[{{- range $i, $item := $items }}{{ if $i }},{{ end }}{{ $item | quote }}{{- end }}]
{{- end }}

{{- /*
Render a single monitor entry as JSON (no trailing comma).
*/ -}}
{{- define "instagram-monitor.monitorEntry" -}}
{
      "username": {{ .username | quote }},
      "displayName": {{ .displayName | default (printf "@%s" .username) | quote }},
      "keywords": {{ include "instagram-monitor.toJsonArray" .keywords }},
      "notifyOnAny": {{ .notifyOnAny | default false | toJson }}
    }
{{- end }}

{{- /*
The config JSON for the monitor.
*/ -}}
{{- define "instagram-monitor.config_json" -}}
{
  "monitors": [
{{- $monitors := .Values.monitors }}
{{- $lastMon := sub (len $monitors) 1 }}
{{- range $i, $m := $monitors }}
    {{- include "instagram-monitor.monitorEntry" $m | nindent 4 }}{{ if ne $i $lastMon }},{{ end }}
{{- end }}
  ],
  "stateFile": "/app/state/state.json",
  "httpTimeout": {{ .Values.httpTimeout | default 30 }}
}
{{- end }}
