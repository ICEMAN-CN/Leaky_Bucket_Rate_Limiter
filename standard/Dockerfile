FROM clojure:temurin-21-tools-deps@sha256:93332755616197432f2f665152fe74828ee745e7cd741832d010d45fd394dc75 AS deps
WORKDIR /app/standard
COPY standard/deps.edn .
RUN mkdir -p src /app/lib && clojure -P && clojure -Spath | tr ':' '\n' | grep '\.jar$' | xargs -I{} cp {} /app/lib/
COPY standard/src src

FROM deps AS test
ENV LANG=C.UTF-8
COPY contracts /app/contracts
COPY standard/test test
CMD ["clojure","-M:test"]

FROM eclipse-temurin:21-jre-alpine@sha256:5d7689f47d670aca766b9503a70bccc2d73b4eb9221ae8f714ae958313c4ce70 AS runtime
WORKDIR /app/standard
COPY --from=deps /app/lib /app/lib
COPY --from=deps /app/standard/src src
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["java","-cp","src:/app/lib/*","clojure.main","-m","limiter.server"]
