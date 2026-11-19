{{/* WAL-G storage settings, shared by the members and the restore check. */}}
{{- define "pgha.walgEnv" -}}
{{- if eq .Values.backup.storage "file" }}
- name: WALG_FILE_PREFIX
  value: {{ include "pgha.backupPath" . | quote }}
{{- else if eq .Values.backup.storage "gs" }}
- name: WALG_GS_PREFIX
  value: {{ .Values.backup.gs.prefix | quote }}
{{- if .Values.backup.gs.existingSecret }}
- name: GOOGLE_APPLICATION_CREDENTIALS
  value: {{ printf "%s/%s" (include "pgha.gcsKeyDir" .) .Values.backup.gs.secretKey | quote }}
{{- end }}
{{- with .Values.backup.gs.endpoint }}
- name: STORAGE_EMULATOR_HOST
  value: {{ . | quote }}
{{- end }}
{{- else }}
- name: WALG_S3_PREFIX
  value: {{ .Values.backup.s3.prefix | quote }}
- name: AWS_REGION
  value: {{ .Values.backup.s3.region | quote }}
{{- with .Values.backup.s3.endpoint }}
- name: AWS_ENDPOINT
  value: {{ . | quote }}
{{- end }}
- name: AWS_S3_FORCE_PATH_STYLE
  value: {{ .Values.backup.s3.forcePathStyle | quote }}
- name: AWS_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.backup.s3.existingSecret }}
      key: AWS_ACCESS_KEY_ID
- name: AWS_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.backup.s3.existingSecret }}
      key: AWS_SECRET_ACCESS_KEY
{{- end }}
{{- range $k, $v := .Values.backup.env }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end -}}

{{/* fileBackup is true when backups go to a mounted volume. */}}
{{- define "pgha.fileBackup" -}}
{{- if and .Values.backup.enabled (eq .Values.backup.storage "file") }}true{{ end -}}
{{- end -}}

{{/* backupPath is the WAL-G file prefix: the mount path plus the prefix
     directory (the cluster name by default). */}}
{{- define "pgha.backupPath" -}}
{{- printf "%s/%s" .Values.backup.file.mountPath (default (include "pgha.name" .) .Values.backup.file.prefix) -}}
{{- end -}}

{{/* backupClaim is the claim holding the file backend. */}}
{{- define "pgha.backupClaim" -}}
{{- default (printf "%s-backup" (include "pgha.name" .)) .Values.backup.file.existingClaim -}}
{{- end -}}

{{/* gcsKey is true when the members read a service account key from a secret. */}}
{{- define "pgha.gcsKey" -}}
{{- if and .Values.backup.enabled (eq .Values.backup.storage "gs") .Values.backup.gs.existingSecret }}true{{ end -}}
{{- end -}}

{{/* gcsAnnotations are the annotations workload identity needs on the service
     accounts; empty otherwise. */}}
{{- define "pgha.gcsAnnotations" -}}
{{- if and .Values.backup.enabled (eq .Values.backup.storage "gs") .Values.backup.gs.serviceAccountAnnotations }}
annotations:
  {{- toYaml .Values.backup.gs.serviceAccountAnnotations | nindent 2 }}
{{- end }}
{{- end -}}

{{/* gcsKeyDir is where the service account key secret is mounted. */}}
{{- define "pgha.gcsKeyDir" -}}/etc/pgha/gcs{{- end -}}

{{/* gcsKeyVolume is the secret volume holding the service account key. */}}
{{- define "pgha.gcsKeyVolume" -}}
- name: gcs-key
  secret:
    secretName: {{ .Values.backup.gs.existingSecret }}
    defaultMode: 0440
{{- end -}}

{{/* gcsKeyMount mounts that volume read-only. */}}
{{- define "pgha.gcsKeyMount" -}}
- name: gcs-key
  mountPath: {{ include "pgha.gcsKeyDir" . }}
  readOnly: true
{{- end -}}

{{/* restoreFile is true when a restore bootstrap reads a file source. */}}
{{- define "pgha.restoreFile" -}}
{{- $r := .Values.bootstrap.restore -}}
{{- if and (eq .Values.bootstrap.mode "restore") (or (eq $r.source "file") (and (not $r.source) (not $r.prefix) (eq .Values.backup.storage "file") $r.file.directory)) }}true{{ end -}}
{{- end -}}

{{/* restoreStorage is the restore source's backend, as the agent reads it. */}}
{{- define "pgha.restoreStorage" -}}
{{- $r := .Values.bootstrap.restore -}}
{{- if include "pgha.restoreFile" . }}file
{{- else if or (eq $r.source "gs") (and (not $r.source) (hasPrefix "gs://" $r.prefix)) }}gs
{{- else if $r.prefix }}s3{{ end -}}
{{- end -}}

{{/* restorePrefix is the source the agent restores from: the S3 or gs prefix, a
     directory on the read-only source volume, or one on the cluster's own
     backup volume. Empty means the cluster's own storage. */}}
{{- define "pgha.restorePrefix" -}}
{{- $r := .Values.bootstrap.restore -}}
{{- if include "pgha.restoreFile" . -}}
{{- if $r.file.existingClaim }}{{ printf "/restore-source/%s" $r.file.directory }}
{{- else if $r.file.directory }}{{ printf "%s/%s" .Values.backup.file.mountPath $r.file.directory }}
{{- end -}}
{{- else -}}
{{- $r.prefix -}}
{{- end -}}
{{- end -}}
