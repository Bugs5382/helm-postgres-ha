{{/* The cluster name: every resource is named after it. */}}
{{- define "pgha.name" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- end -}}

{{- define "pgha.headless" -}}{{ include "pgha.name" . }}-headless{{- end -}}
{{- define "pgha.primary" -}}{{ include "pgha.name" . }}-primary{{- end -}}
{{- define "pgha.replica" -}}{{ include "pgha.name" . }}-replica{{- end -}}
{{- define "pgha.pgbouncer" -}}{{ include "pgha.name" . }}-pgbouncer{{- end -}}
{{- define "pgha.backupLease" -}}{{ include "pgha.name" . }}-backup{{- end -}}
{{- define "pgha.tlsSecret" -}}{{ default (printf "%s-tls" (include "pgha.name" .)) .Values.tls.existingSecret }}{{- end -}}
{{- define "pgha.pgbouncerTLSSecret" -}}
{{- if .Values.tls.existingSecret -}}{{ default .Values.tls.existingSecret .Values.tls.pgbouncerExistingSecret }}{{- else -}}{{ include "pgha.name" . }}-pgbouncer-tls{{- end -}}
{{- end -}}

{{- define "pgha.selectorLabels" -}}
app.kubernetes.io/name: postgres-ha
app.kubernetes.io/instance: {{ include "pgha.name" . }}
{{- end -}}

{{- define "pgha.labels" -}}
{{ include "pgha.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "pgha.pgbouncerSelectorLabels" -}}
app.kubernetes.io/name: postgres-ha-pgbouncer
app.kubernetes.io/instance: {{ include "pgha.name" . }}
{{- end -}}

{{/* image renders registry/repository[:tag][@digest]. */}}
{{- define "pgha.image" -}}
{{- $img := .image -}}
{{- $tag := default .defaultTag $img.tag -}}
{{- printf "%s/%s" $img.registry $img.repository -}}
{{- if $tag }}:{{ $tag }}{{ end -}}
{{- if $img.digest }}@{{ $img.digest }}{{ end -}}
{{- end -}}

{{- define "pgha.postgresImage" -}}{{ include "pgha.image" (dict "image" .Values.image "defaultTag" "") }}{{- end -}}
{{- define "pgha.agentImage" -}}{{ include "pgha.image" (dict "image" .Values.agent.image "defaultTag" .Chart.Version) }}{{- end -}}
{{- define "pgha.postgresExporterImage" -}}{{ include "pgha.image" (dict "image" .Values.metrics.exporters.postgres.image "defaultTag" "") }}{{- end -}}
{{- define "pgha.pgbouncerExporterImage" -}}{{ include "pgha.image" (dict "image" .Values.metrics.exporters.pgbouncer.image "defaultTag" "") }}{{- end -}}
{{- define "pgha.pgbouncerImage" -}}{{ include "pgha.image" (dict "image" .Values.pgbouncer.image "defaultTag" "") }}{{- end -}}

{{/* Credential Secret names: the user's existingSecret, or the generated one. */}}
{{- define "pgha.credSecret" -}}
{{- $c := index .root.Values.credentials .name -}}
{{- default (printf "%s-%s" (include "pgha.name" .root) .name) $c.existingSecret -}}
{{- end -}}
{{- define "pgha.credKey" -}}
{{- $c := index .root.Values.credentials .name -}}
{{- default "password" $c.passwordKey -}}
{{- end -}}
{{- define "pgha.roleSecret" -}}
{{- default (printf "%s-role-%s" (include "pgha.name" .root) .role.name) .role.existingSecret -}}
{{- end -}}

{{/* The credentials the chart uses, in the order the agent reads them. */}}
{{- define "pgha.credentialNames" -}}
{{- $names := list "superuser" "replication" "rewind" -}}
{{- if .Values.pgbouncer.enabled }}{{ $names = append $names "pgbouncer" }}{{ end -}}
{{- toJson $names -}}
{{- end -}}

{{- define "pgha.usernames" -}}
{"superuser":"postgres","replication":"replicator","rewind":"pgha_rewind","pgbouncer":"pgbouncer_auth"}
{{- end -}}

{{/* Settings the chart owns. Users cannot override them through parameters. */}}
{{- define "pgha.ownedSettings" -}}
{{- list "primary_conninfo" "primary_slot_name" "restore_command" "recovery_target" "recovery_target_time" "recovery_target_lsn" "recovery_target_name" "recovery_target_xid" "recovery_target_action" "recovery_target_inclusive" "recovery_target_timeline" "archive_mode" "archive_command" "ssl" "ssl_cert_file" "ssl_key_file" "ssl_ca_file" "listen_addresses" "port" "unix_socket_directories" "hba_file" "config_file" "data_directory" "wal_level" "hot_standby" "wal_log_hints" "synchronous_standby_names" "password_encryption" "tcp_user_timeout" "ident_file" "include" "include_if_exists" "include_dir" | toJson -}}
{{- end -}}

{{/* memoryMB turns a Kubernetes memory quantity into megabytes (MiB). */}}
{{- define "pgha.memoryMB" -}}
{{- $q := toString . -}}
{{- $mb := 0.0 -}}
{{- if hasSuffix "Gi" $q }}{{ $mb = mulf (float64 (trimSuffix "Gi" $q)) 1024 }}
{{- else if hasSuffix "Mi" $q }}{{ $mb = float64 (trimSuffix "Mi" $q) }}
{{- else if hasSuffix "Ki" $q }}{{ $mb = divf (float64 (trimSuffix "Ki" $q)) 1024 }}
{{- else if hasSuffix "G" $q }}{{ $mb = divf (mulf (float64 (trimSuffix "G" $q)) 1000000000) 1048576 }}
{{- else if hasSuffix "M" $q }}{{ $mb = divf (mulf (float64 (trimSuffix "M" $q)) 1000000) 1048576 }}
{{- else if $q }}{{ $mb = divf (float64 $q) 1048576 }}
{{- end -}}
{{- int64 $mb -}}
{{- end -}}

{{/* parameters are the postgresql.conf settings from values, with
     shared_buffers (a quarter) and effective_cache_size (three quarters)
     sized from the container's memory when they are not set. */}}
{{- define "pgha.parameters" -}}
{{- $p := deepCopy .Values.postgresql.parameters -}}
{{- $mem := "" -}}
{{- with .Values.resources }}{{ with .limits }}{{ $mem = default "" .memory }}{{ end }}{{ if not $mem }}{{ with .requests }}{{ $mem = default "" .memory }}{{ end }}{{ end }}{{ end -}}
{{- if $mem -}}
{{- $mb := int64 (include "pgha.memoryMB" $mem) -}}
{{- if not (hasKey $p "shared_buffers") }}{{ $_ := set $p "shared_buffers" (printf "%dMB" (div $mb 4)) }}{{ end -}}
{{- if not (hasKey $p "effective_cache_size") }}{{ $_ := set $p "effective_cache_size" (printf "%dMB" (div (mul $mb 3) 4)) }}{{ end -}}
{{- end -}}
{{- toJson $p -}}
{{- end -}}

{{/* Parameters that need a restart; the pods roll when one changes. */}}
{{- define "pgha.restartChecksum" -}}
{{- $restart := list "max_connections" "shared_buffers" "max_wal_senders" "max_replication_slots" "max_worker_processes" "max_prepared_transactions" "max_locks_per_transaction" "shared_preload_libraries" "huge_pages" "wal_buffers" -}}
{{- $out := dict "archive" .Values.backup.enabled "image" (include "pgha.postgresImage" .) "replicas" .Values.replicas -}}
{{- range $k, $v := include "pgha.parameters" . | fromJson }}{{ if has $k $restart }}{{ $_ := set $out $k $v }}{{ end }}{{ end -}}
{{- toJson $out | sha256sum -}}
{{- end -}}
