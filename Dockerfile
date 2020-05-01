FROM alpine:latest
COPY cli53 /bin/cli53
RUN chmod +x /bin/cli53 && apk add --no-cache bash openssl ca-certificates
ENTRYPOINT ["cli53"]
CMD ["-v"]