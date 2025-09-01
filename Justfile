# Variables (override by: just VAR=value target)
REMOTE_HOST := 'coolify'
REMOTE_DIR := '~/apps/go-postgres-app'
APP_NAME   := 'go-postgres-app'
NAMESPACE  := 'default'

# Container registry to build/push from the VM (optional)
# Example: your.docker.registry, or leave empty and override IMAGE directly
REGISTRY   := ''
PUSH       := 'false'

# CloudNativePG cluster integration
PG_CLUSTER := 'pg-demo'
PG_SECRET  := 'pg-demo-superuser'  # override to pg-demo-app if present
PG_DB      := 'appdb'

# Derived
REMOTE_REPO   := 'https://github.com/vramana/go-postgres-app.git'
REMOTE_BRANCH := 'master'

_ensure-remote:
	#!/usr/bin/env bash
	ssh {{REMOTE_HOST}} bash -se <<'SSH'
	set -euxo pipefail
	mkdir -p ~/apps
	SSH

# 1) Publish: commit dirty changes with jj and push to GitHub
publish:
	#!/usr/bin/env bash
	set -euo pipefail
	if git status --porcelain | grep -q .; then
		echo ':: Detected local changes; committing with jj'
		jj commit -m "deploy: $(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	else
		echo ':: No local changes to commit'
	fi
	echo ':: Pushing current change via jj'
	jj bookmark set master -r @-
	jj git push

# 3) Remote build image and push to registry (must be logged in on VM)
remote-build: remote-sync
	#!/usr/bin/env bash
	ssh {{REMOTE_HOST}} bash -se <<'SSH'
	set -euxo pipefail
	cd {{REMOTE_DIR}}
	docker build -t {{APP_NAME}} .
	SSH

# 4) Update image in kustomization and deploy to cluster
remote-deploy: remote-build
	#!/usr/bin/env bash
	ssh {{REMOTE_HOST}} bash -se <<'SSH'
	set -euxo pipefail
	cd {{REMOTE_DIR}}/k8s
	sed -i.bak -E "s/(value: )pg-demo-rw/\\1{{PG_CLUSTER}}-rw/" deployment.yaml || true
	sed -i.bak -E "s/(name: )pg-demo-superuser/\\1{{PG_SECRET}}/" deployment.yaml || true
	sed -i.bak -E "s/(value: )appdb/\\1{{PG_DB}}/" deployment.yaml || true
	rm -f deployment.yaml.bak
	kubectl -n {{NAMESPACE}} apply -k .
	IMAGE=$(cat ../.last-image | cut -d= -f2)
	echo ":: Setting deployment image to $IMAGE"
	kubectl -n {{NAMESPACE}} set image deploy/{{APP_NAME}} app="$IMAGE"
	kubectl -n {{NAMESPACE}} rollout status deploy/{{APP_NAME}} --timeout=120s
	SSH

# 5) Single entrypoint to package, upload, build, and deploy
remote-sync: _ensure-remote
	#!/usr/bin/env bash
	ssh {{REMOTE_HOST}} bash -se <<'SSH'
	set -euxo pipefail
	cd ~/apps
	if [ ! -d {{REMOTE_DIR}}/.git ]; then
	  echo ':: Cloning repository'
	  git clone "{{REMOTE_REPO}}"
	  cd {{REMOTE_DIR}}
	else
	  echo ':: Updating repository'
	  cd {{REMOTE_DIR}}
	  git remote set-url origin "{{REMOTE_REPO}}"
	  git pull origin master
	fi
	SSH

# 5) Single entrypoint to publish, pull, build, and deploy
deploy: publish remote-sync remote-build remote-deploy
	@echo ':: Deployed {{APP_NAME}} to namespace {{NAMESPACE}}'

# Useful: tail logs
logs:
	#!/usr/bin/env bash
	ssh {{REMOTE_HOST}} bash -se <<'SSH'
	kubectl -n {{NAMESPACE}} logs deploy/{{APP_NAME}} -f --tail=200
	SSH

# Useful: port-forward for quick local access from the VM
port-forward:
	#!/usr/bin/env bash
	ssh {{REMOTE_HOST}} bash -se <<'SSH'
	kubectl -n {{NAMESPACE}} port-forward svc/{{APP_NAME}} 8080:80
	SSH
