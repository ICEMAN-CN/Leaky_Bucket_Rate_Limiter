FROM golang:1.25.5-alpine AS build
WORKDIR /src
COPY console/ .
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -o /out/console .
FROM build AS test
RUN --mount=type=cache,target=/root/.cache/go-build go test -c -o /out/console.test .
CMD ["/out/console.test", "-test.v"]
FROM alpine:3.22.2
COPY --from=build /out/console /usr/local/bin/console
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["console"]
