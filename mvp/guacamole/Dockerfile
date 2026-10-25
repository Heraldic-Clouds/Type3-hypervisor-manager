FROM maven:3.9.11-eclipse-temurin-17 AS extension
WORKDIR /src
COPY guacamole/pom.xml .
COPY guacamole/src src
RUN mvn -B -q package
FROM guacamole/guacamole:1.6.0@sha256:f344085e618bb05e22b964b0208dbd06d3468275bac70206f93805245e067b40
USER root
RUN mkdir -p /etc/guacamole/extensions
COPY --from=extension /src/target/mea-lifecycle-1.0.0.jar /etc/guacamole/extensions/mea-lifecycle.jar
COPY schema/rds-ca.pem /opt/guacamole/rds-ca.pem
ENV GUACAMOLE_HOME=/etc/guacamole
USER guacamole
