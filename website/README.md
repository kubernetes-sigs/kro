# Website

This website is built using [Docusaurus](https://docusaurus.io/), a modern static website generator.

### Installation
If you're using other tools than `npm`, you can refer to the `docusaurs documentation` for build and installation https://docusaurus.io/docs/installation#build

If you are using `yarn`, use:

```
$ yarn
```
### Local Development

To start a local development server, use:

```
$ yarn start
```

This command starts a local development server and opens up a browser window. Most changes are reflected live without having to restart the server.

### Build

To generate static content for deployment, use:

```
$ yarn build
```

This command generates static content into the `build` directory and can be served using any static contents hosting service.

### Deployment

Using SSH:

```
$ USE_SSH=true yarn deploy
```

Not using SSH:

```
$ GIT_USER=<Your GitHub username> yarn deploy
```

If you are using GitHub pages for hosting, this command is a convenient way to build the website and push to the `gh-pages` branch.
