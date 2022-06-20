FROM bitnami/kubectl:1.21

USER root

RUN /bin/bash -c "curl -L https://github.com/SwissDataScienceCenter/taweret/releases/download/v0.1.1/taweret_0.1.1_Linux_x86_64.tar.gz | tar xvz -C /usr/local/bin/; \
curl -L https://github.com/kanisterio/kanister/releases/download/0.78.0/kanister_0.78.0_linux_amd64.tar.gz | tar xvz -C /usr/local/bin/"

USER 1001

EXPOSE 2112/tcp

ENTRYPOINT []

CMD /usr/local/bin/taweret 
