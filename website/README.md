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

### Versioning

When a new kro controller version is released, you should also update/release the documentation:

1. **Create a Docusaurus version**  
    Remove the `v` prefix from your version (e.g., `v0.1.0` → `0.1.0`):

    ```shell
    version_number=${version#v}
    npm run docusaurus docs:version $version_number
    ```

2. **Choose whether to promote the docs to latest**
    `lastVersion` in `docusaurus.config.ts` controls which version is served at
    the unversioned URLs and recommended as latest. Update it only when
    promoting a stable release.

    For a release candidate, use the full version without the `v` prefix, e.g.
    `npm run docusaurus docs:version 0.10.0-rc.0`, and leave `lastVersion` at
    `0.9.4`. Keep the new entry at the start of `versions.json`: Docusaurus
    serves it under `/0.10.0-rc.0/`, includes it in the version dropdown, and
    displays its built-in unreleased banner with a link to the latest stable
    docs. The development docs remain under `/next/`.

3. **Review version-specific instructions**
    The version command copies `docs/` and the sidebars; it does not update
    installation commands. In the new snapshot, pin install and upgrade
    examples to the release being documented. For RCs, an unpinned Helm command
    or GitHub's `releases/latest` endpoint selects stable instead of the RC.
    Keep the stable docs and their installation instructions unchanged.

4. **Add redirects for moved pages (when promoting stable 0.10.0)**
    The current docs reorganized `docs/concepts/` ahead of 0.10.0. Once 0.10.0
    becomes `lastVersion` in `docusaurus.config.ts`, the pre-0.10.0 URLs below
    stop resolving at the site root. Add `@docusaurus/plugin-client-redirects`
    with these entries in the same PR. They cannot be added earlier because the
    `from` paths are still live routes served by the 0.9.x version.

    | From | To |
    | --- | --- |
    | `/docs/concepts/instances` | `/docs/concepts/rgd/instances` |
    | `/docs/concepts/rgd/cel-expressions` | `/docs/concepts/expressions/cel-expressions` |
    | `/docs/concepts/rgd/cel-libraries` | `/docs/concepts/expressions/cel-libraries` |
    | `/docs/concepts/rgd/dependencies-ordering` | `/docs/concepts/expressions/dependencies-ordering` |
    | `/docs/concepts/rgd/resource-definitions/resource-basics` | `/docs/concepts/rgd/resource-basics` |
    | `/docs/concepts/rgd/resource-definitions/conditional-creation` | `/docs/concepts/reconciliation/conditional-creation` |
    | `/docs/concepts/rgd/resource-definitions/readiness` | `/docs/concepts/reconciliation/readiness` |
    | `/docs/concepts/rgd/resource-definitions/collections` | `/docs/concepts/reconciliation/collections` |
    | `/docs/concepts/rgd/resource-definitions/external-references` | `/docs/concepts/reconciliation/external-references` |

5. **Validate and submit a Pull Request**
    Run `npm run typecheck`, `npm test`, and `npm run build` from `website/`.
    Check that the new snapshot has its own routes, that its internal links
    stay in that version, and that the homepage and latest-version banner
    links still target the stable docs. Commit the snapshot, versioned sidebar,
    `versions.json`, and any supporting changes, then open a PR. The docs
    deployment workflow publishes all versions when the PR merges into `main`;
    a release tag is not required to publish the docs.
