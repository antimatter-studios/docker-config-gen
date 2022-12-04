FROM node:alpine
LABEL maintainer="Chris Thomas <chris.alex.thomas@gmail.com> (@chrisalexthomas)"

ARG RUN_YARN=true

ENV CONFIG_PATH=/config

COPY . /app/
WORKDIR /app/

RUN if [ "${RUN_YARN}" = "true" ]; then \
        apk add --no-cache --virtual .build-deps python3 make cmake g++; \
        echo "Installing packages"; yarn --frozen-lockfile; \
        apk del .build-deps; \
    else \
        echo "Skipping yarn install because RUN_YARN='${RUN_YARN}'"; \
    fi

CMD ["yarn", "start"]