import type { Config } from "@docusaurus/types";
import type * as Preset from "@docusaurus/preset-classic";
import { kroLight, kroDark } from "./src/prism/kroTheme";

const config: Config = {
  title: "kro",
  tagline: "Kube Resource Orchestrator",
  favicon: "img/favicon.ico",
  plugins: [
    require.resolve("docusaurus-lunr-search"),
    require.resolve("docusaurus-plugin-image-zoom"),
    function rawYamlPlugin() {
      return {
        name: "raw-yaml-plugin",
        configureWebpack() {
          return {
            module: {
              rules: [
                {
                  test: /\.yaml$/,
                  type: "asset/source",
                },
              ],
            },
          };
        },
      };
    },
  ],
  // Set the production url of your site here
  url: "https://kro.run",
  // Set the /<baseUrl>/ pathname under which your site is served
  // For GitHub pages deployment, it is often '/<projectName>/'
  baseUrl: "/",

  // GitHub pages deployment config.
  // If you aren't using GitHub pages, you don't need these.
  organizationName: "kubernetes-sigs", // Usually your GitHub org/user name.
  projectName: "kro", // Usually your repo name.

  onBrokenLinks: "throw",
  markdown: {
    hooks: {
      onBrokenMarkdownLinks: "warn",
    },
    mermaid: true,
  },
  themes: ["@docusaurus/theme-mermaid"],

  // Even if you don't use internationalization, you can use this field to set
  // useful metadata like html lang. For example, if your site is Chinese, you
  // may want to replace "en" with "zh-Hans".
  i18n: {
    defaultLocale: "en",
    locales: ["en"],
  },
  headTags: [
    {
      tagName: "meta",
      attributes: {
        name: "go-import",
        content: "kro.run/pkg git https://github.com/kubernetes-sigs/kro",
      },
    },
    {
      tagName: "meta",
      attributes: {
        name: "go-source",
        content:
          "kro.run/pkg git https://github.com/kubernetes-sigs/kro https://github.com/kubernetes-sigs/kro/tree/main{/dir} https://github.com/kubernetes-sigs/kro/blob/main{/dir}/{file}#L{line}",
      },
    },
  ],

  presets: [
    [
      "classic",
      {
        docs: {
          routeBasePath: "/",

          sidebarPath: "./sidebars.ts",
          versions: {
            current: {
              label: "main",
            },
          },
          // sidebarCollapsed: false,
          // Please change this to your repo.
          // Remove this to remove the "edit this page" links.
          editUrl: "https://github.com/kubernetes-sigs/kro/tree/main/website",
          disableVersioning: false,
          includeCurrentVersion: true,
          lastVersion: "0.8.3",
        },
        blog: false,
        theme: {
          customCss: "./src/css/custom.css",
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    // Replace with your project's social card
    image: "img/kro.svg",
    docs: {
      sidebar: {
        hideable: false,
        autoCollapseCategories: false,
      },
    },
    navbar: {
      title: "kro",
      hideOnScroll: true,
      items: [
        {
          type: "docSidebar",
          sidebarId: "docsSidebar",
          position: "left",
          label: "Documentation",
        },
        {
          type: "docSidebar",
          sidebarId: "apisSidebar",
          position: "left",
          label: "Reference",
        },
        {
          type: "docSidebar",
          sidebarId: "examplesSidebar",
          position: "left",
          label: "Examples",
        },
        {
          type: "docsVersionDropdown",
          position: "right",
          dropdownActiveClassDisabled: true,
          dropdownItemsAfter: [
            {
              type: "html",
              value: '<hr class="dropdown-separator">',
            },
            {
              to: "/versions",
              label: "All versions",
            },
          ],
        },
        {
          href: "https://github.com/kubernetes-sigs/kro",
          position: "right",
          className: "header-github-link",
          "aria-label": "GitHub repository",
        },
      ],
    },
    footer: {
      style: "dark",
      links: [
        {
          title: "Docs",
          items: [
            {
              label: "Tutorial",
              to: "/docs/overview",
            },
            {
              label: "Examples",
              to: "/examples/",
            },
          ],
        },
        {
          title: "Community",
          items: [
            {
              label: "Slack",
              href: "https://kubernetes.slack.com/archives/C081TMY9D6Y",
            },
            {
              label: "Contribution Guide",
              href: "https://github.com/kubernetes-sigs/kro/blob/main/CONTRIBUTING.md",
            },
          ],
        },
        {
          title: "More",
          items: [
            {
              label: "GitHub",
              href: "https://github.com/kubernetes-sigs/kro",
            },
            {
              label: "YouTube",
              href: "https://www.youtube.com/channel/UCUlcI3NYq9ehl5wsdfbJzSA",
            },
          ],
        },
      ],
      copyright:
        "kro is a subproject of Kubernetes SIG Cloud Provider. Kubernetes is a CNCF graduated project.",
    },
    prism: {
      theme: kroLight,
      darkTheme: kroDark,
      additionalLanguages: ["bash", "yaml"],
    },
    zoom: {
      selector: ".markdown img",
      background: {
        light: "rgb(255, 255, 255)",
        dark: "rgb(30, 30, 30)",
      },
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
