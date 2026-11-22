FROM golang:1.25.5-alpine AS source
WORKDIR /src/distributed
COPY distributed/go.mod distributed/go.sum ./
RUN go mod download
COPY distributed/ ./

FROM source AS test
RUN apk add --no-cache redis gcc musl-dev
RUN --mount=type=cache,target=/root/.cache/go-build go test -race -c -o /usr/local/bin/limiter-tests .
COPY contracts/ /src/contracts/
CMD ["/usr/local/bin/limiter-tests", "-test.v", "-test.timeout=2m"]

FROM source AS build
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/limiter .

FROM alpine:3.22.2
COPY --from=build /out/limiter /usr/local/bin/limiter
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/limiter"]
