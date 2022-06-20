FROM bitnami/kubectl:1.21

USER root

RUN /bin/bash -c "curl -L https://github.com/SwissDataScienceCenter/taweret/releases/download/v0.2.0-beta/taweret_0.2.0-beta_Linux_x86_64.tar.gz | tar xvz -C /usr/local/bin/; \
curl -L https://github.com/kanisterio/kanister/releases/download/0.78.0/kanister_0.78.0_linux_amd64.tar.gz | tar xvz -C /usr/local/bin/"

USER 1001

EXPOSE 2112/tcp

ENTRYPOINT []

CMD /usr/local/bin/taweret 


#--daily-backups $DAILY_BACKUPS --weekly-backups $WEEKLY_BACKUPS --kanister-namespace $KANISTER_NAMESPACE --blueprint-namespace $BLUEPRINT_NAMESPACE --s3-profile $S3_PROFILE --eval-schedule $EVAL_SCHEDULE
