{{- define "zstack-csi.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "zstack-csi.secretName" -}}
{{ .Values.zstack.existingSecret | default (printf "%s-zstack" .Release.Name) }}
{{- end }}

{{- define "zstack-csi.socketDir" -}}
{{ .Values.node.kubeletDir }}/plugins/{{ .Values.driverName }}
{{- end }}
