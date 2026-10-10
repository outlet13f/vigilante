{{- define "vigilante.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "vigilante.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "vigilante.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "vigilante.serviceAccount" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "vigilante.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "vigilante.configMap" -}}
{{- default (include "vigilante.fullname" .) .Values.existingConfigMap -}}
{{- end -}}

{{/*
The inline config as YAML of a map (empty with existingConfigMap). Rendering
fails when it is not valid YAML.
*/}}
{{- define "vigilante.config" -}}
{{- if .Values.existingConfigMap -}}
{{- dict | toYaml -}}
{{- else -}}
{{- $cfg := fromYaml .Values.config -}}
{{- if hasKey $cfg "Error" -}}
{{- fail (printf "config is not valid YAML: %v" (get $cfg "Error")) -}}
{{- end -}}
{{- $cfg | toYaml -}}
{{- end -}}
{{- end -}}

{{/* (list m key): m.key as YAML when it is a map, else an empty map. */}}
{{- define "vigilante.section" -}}
{{- $v := dict -}}
{{- if kindIs "map" (index . 0) -}}{{- $v = get (index . 0) (index . 1) -}}{{- end -}}
{{- if kindIs "map" $v -}}{{- $v | toYaml -}}{{- else -}}{{- dict | toYaml -}}{{- end -}}
{{- end -}}

{{/*
"true" when the pod serves HTTPS itself: tls.enabled, or server.tls.cert_file
in the inline config.
*/}}
{{- define "vigilante.tls" -}}
{{- $cfg := include "vigilante.config" . | fromYaml -}}
{{- $server := include "vigilante.section" (list $cfg "server") | fromYaml -}}
{{- $tls := include "vigilante.section" (list $server "tls") | fromYaml -}}
{{- if or (default (dict) .Values.tls).enabled (get $tls "cert_file") -}}true{{- end -}}
{{- end -}}

{{- define "vigilante.scheme" -}}
{{- if include "vigilante.tls" . -}}https{{- else -}}http{{- end -}}
{{- end -}}

{{/*
Checks rendered with the Deployment: authentication is configured (or
anonymous access accepted explicitly), and the TLS settings work in a pod.
The chart cannot read an existingConfigMap, so it checks only `config`.
*/}}
{{- define "vigilante.validate" -}}
{{- if not .Values.existingConfigMap -}}
{{- $cfg := include "vigilante.config" . | fromYaml -}}
{{- $server := include "vigilante.section" (list $cfg "server") | fromYaml -}}
{{- $auth := include "vigilante.section" (list $cfg "auth") | fromYaml -}}
{{- $oidc := include "vigilante.section" (list $auth "oidc") | fromYaml -}}
{{- $tls := include "vigilante.section" (list $server "tls") | fromYaml -}}
{{- $accounts := get $auth "service_accounts" -}}
{{- $tokenEnv := get $server "auth_token_env" | toString -}}
{{- $hasAuth := false -}}
{{- if and (kindIs "slice" $accounts) $accounts -}}{{- $hasAuth = true -}}{{- end -}}
{{- if get $oidc "issuer" -}}{{- $hasAuth = true -}}{{- end -}}
{{- if and (not $hasAuth) $tokenEnv -}}
{{- /* The break-glass token counts only when the pod gets the variable. */ -}}
{{- $named := false -}}
{{- range .Values.env -}}{{- if eq (toString .name) $tokenEnv -}}{{- $named = true -}}{{- end -}}{{- end -}}
{{- if not (or $named .Values.envFrom) -}}
{{- fail (printf "config sets server.auth_token_env: %s, but env / envFrom do not provide %s (e.g. from a Secret), so the server would run without authentication" $tokenEnv $tokenEnv) -}}
{{- end -}}
{{- $hasAuth = true -}}
{{- end -}}
{{- if and (not $hasAuth) (not (default (dict) .Values.auth).allowAnonymous) -}}
{{- fail "config has no authentication, so every API caller would be an anonymous admin. Configure auth.service_accounts (token hashes from `vigilante token create`), auth.oidc or server.auth_token_env in config (see values.yaml), or set auth.allowAnonymous=true for a development install" -}}
{{- end -}}
{{- if and (default (dict) .Values.tls).enabled (not (get $tls "cert_file")) -}}
{{- fail "tls.enabled is set but config has no server.tls.cert_file: the server would serve plain HTTP while the probes and HA forwarding use HTTPS" -}}
{{- end -}}
{{- if eq (toString (get $tls "client_auth")) "require" -}}
{{- fail "config sets server.tls.client_auth: require, which kubelet probes and HA forwarding cannot satisfy (they present no client certificate): use optional" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
GOMEMLIMIT: goMemLimit, or 90% of a whole Mi/Gi/M/G memory limit. Empty when
it is off or has to come from resourceFieldRef (other quantity forms).
*/}}
{{- define "vigilante.memoryLimit" -}}
{{- if and (kindIs "map" .Values.resources) (kindIs "map" .Values.resources.limits) -}}
{{- toString (default "" (get .Values.resources.limits "memory")) -}}
{{- end -}}
{{- end -}}

{{- define "vigilante.goMemLimit" -}}
{{- $v := toString (default "" .Values.goMemLimit) -}}
{{- $l := include "vigilante.memoryLimit" . -}}
{{- if eq $v "off" -}}
{{- else if $v -}}
{{- $v -}}
{{- else if regexMatch "^[0-9]+Gi$" $l -}}
{{- div (mul (trimSuffix "Gi" $l | atoi) 1024 9) 10 -}}MiB
{{- else if regexMatch "^[0-9]+Mi$" $l -}}
{{- div (mul (trimSuffix "Mi" $l | atoi) 9) 10 -}}MiB
{{- else if regexMatch "^[0-9]+G$" $l -}}
{{- div (mul (trimSuffix "G" $l | atoi) 1000000000 9) 10 -}}
{{- else if regexMatch "^[0-9]+M$" $l -}}
{{- div (mul (trimSuffix "M" $l | atoi) 1000000 9) 10 -}}
{{- end -}}
{{- end -}}
