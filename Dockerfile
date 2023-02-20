FROM --platform=linux/amd64 golang:1.18.4-alpine3.16 AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/taweret .

FROM bitnami/kubectl:1.24 AS bin
COPY --from=build /out/taweret /usr/local/bin/

USER root

RUN /bin/bash -c "curl -L https://github.com/kanisterio/kanister/releases/download/0.89.0/kanister_0.89.0_linux_amd64.tar.gz | tar xvz -C /usr/local/bin/"

USER 1001

EXPOSE 2112/tcp

ENV DAILY_BACKUPS=7
ENV WEEKLY_BACKUPS=4
ENV KANISTER_NAMESPACE=kanister
ENV BLUEPRINT_NAME=blueprint
ENV S3_PROFILE_NAME=s3profile

ENTRYPOINT []

CMD /usr/local/bin/taweret
