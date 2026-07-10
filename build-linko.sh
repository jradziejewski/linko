go build \
-ldflags "-X jradziejewski/linko/internal/build.GitSHA=$(git rev-parse HEAD) -X jradziejewski/linko/internal/build.BuildTime=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
-o linko

