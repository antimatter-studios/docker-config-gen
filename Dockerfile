FROM node:alpine
LABEL maintainer="Chris Thomas <chris.alex.thomas@gmail.com> (@chrisalexthomas)"

COPY . /app/
WORKDIR /app/

RUN apk add --no-cache --virtual .build-deps python3 make cmake g++; \
    echo "Installing packages"; yarn --frozen-lockfile; \
    apk del .build-deps;

CMD ["yarn", "start"]