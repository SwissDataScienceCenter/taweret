FROM --platform=linux/amd64 golang:1.20.1-alpine3.17 AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/taweret .

FROM alpine:3.17.2 AS bin
COPY --from=build /out/taweret /usr/local/bin/

EXPOSE 2112/tcp

ENV DAILY_BACKUPS=7
ENV WEEKLY_BACKUPS=4
ENV KANISTER_NAMESPACE=kanister
ENV BLUEPRINT_NAME=blueprint
ENV S3_PROFILE_NAME=s3profile

ENTRYPOINT []

CMD /usr/local/bin/taweret
