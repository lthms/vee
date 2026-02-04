FROM ghcr.io/lthms/vee:devenv

# Cache-bust: Docker re-fetches this URL on every build.
# If main has new commits, the JSON changes -> git pull runs.
# If no new commits, layer cache is reused -> instant build.
ADD https://api.github.com/repos/lthms/vee/git/refs/heads/main /tmp/vee-ref.json
RUN git -C /workspace pull --ff-only && go -C /workspace mod download

WORKDIR /workspace
