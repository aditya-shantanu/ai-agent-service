{{/* Resolve the platform secret name (existing or chart-managed). */}}
{{- define "hermes.platformSecretName" -}}
{{- .Values.secrets.platform.existingSecret | default "hermes-platform-secrets" -}}
{{- end -}}

{{- define "hermes.adminSecretName" -}}
{{- .Values.secrets.admin.existingSecret | default "hermes-gateway-admin" -}}
{{- end -}}

{{- define "hermes.providerSecretName" -}}
{{- .Values.secrets.providerKeys.existingSecret | default "hermes-provider-keys" -}}
{{- end -}}

{{/*
Generate-or-preserve a random secret value: on first install a fresh random
is minted; on upgrade the existing value is kept (lookup). Usage:
  {{ include "hermes.stickySecretValue" (dict "ns" .Release.Namespace "name" "sec" "key" "k" "len" 48) }}
*/}}
{{- define "hermes.stickySecretValue" -}}
{{- $existing := lookup "v1" "Secret" .ns .name -}}
{{- if and $existing (hasKey $existing.data .key) -}}
{{- index $existing.data .key | b64dec -}}
{{- else -}}
{{- randAlphaNum (.len | int) -}}
{{- end -}}
{{- end -}}
