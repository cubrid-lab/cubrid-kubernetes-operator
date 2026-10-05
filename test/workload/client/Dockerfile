# Test-only image: the SQL workload client of docs/testing/scenario-contract.md
# on the official CUBRID JDBC driver. It is never a release image.

ARG JDK_IMAGE=eclipse-temurin:17-jdk
ARG JRE_IMAGE=eclipse-temurin:17-jre

FROM ${JDK_IMAGE} AS build
# The driver is pinned by version and checksum (docs/third-party-material.md).
ARG JDBC_VERSION=11.3.2.0053
ARG JDBC_SHA256=ab8ab5632382a3042c0f354dd903d53b4f15c67b462afbeddb4edcf0bf43ad1f
WORKDIR /build
# Readable by the unprivileged user of the final image: a file fetched by ADD
# is otherwise owner-only, and the driver would not be found.
ADD --checksum=sha256:${JDBC_SHA256} --chmod=0644 \
    https://repo1.maven.org/maven2/org/cubrid/cubrid-jdbc/${JDBC_VERSION}/cubrid-jdbc-${JDBC_VERSION}.jar \
    cubrid-jdbc.jar
COPY test/workload/client/Workload.java .
RUN javac -Xlint:all -Werror -d classes -cp cubrid-jdbc.jar Workload.java \
    && printf '#!/bin/sh\nexec java -cp /app/classes:/app/cubrid-jdbc.jar Workload "$@"\n' > workload \
    && chmod 0755 workload

FROM ${JRE_IMAGE}
COPY --from=build /build/classes /app/classes
COPY --from=build /build/cubrid-jdbc.jar /app/cubrid-jdbc.jar
COPY --from=build /build/workload /usr/local/bin/workload
USER 65532:65532
ENTRYPOINT ["workload"]
