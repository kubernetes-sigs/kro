import { themes as prismThemes } from "prism-react-renderer";
import type { Config } from "@docusaurus/types";
import type * as Preset from "@docusaurus/preset-classic";

const config: Config = {
  title: "kro",
  tagline: "Kube Resource Orchestrator",
  // The Melodious Kubernetes Integrator
  // Cementing Your Kubernetes Infrastructure
  // Connecting the Dots in Your Kubernetes Environment
  // Bringing Cohesion to Your Kubernetes Clusters
  favicon: "img/favicon.ico",
  plugins: [require.resolve("docusaurus-lunr-search")],
  // Set the production url of your site here
  url: "https://kro.run",
  // Set the /<baseUrl>/ pathname under which your site is served
  // For GitHub pages deployment, it is often '/<projectName>/'
  baseUrl: "/",

  // GitHub pages deployment config.
  // If you aren't using GitHub pages, you don't need these.
  organizationName: "awslabs", // Usually your GitHub org/user name.
  projectName: "kro", // Usually your repo name.

  onBrokenLinks: "throw",
  markdown: {
    hooks: {
      onBrokenMarkdownLinks: "warn",
    },
  },

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
          lastVersion: "0.6.0",
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
      /* logo: {
        alt: "KRO Logo",
        src: "img/kro-light.svg",
        srcDark: "img/kro-dark.svg",
      }, */
      items: [
        {
          type: "docSidebar",
          sidebarId: "docsSidebar",
          position: "left",
          label: "Documentation",
        },
        {
          type: "docSidebar",
          sidebarId: "examplesSidebar",
          position: "left",
          label: "Examples",
        },
        /* {
          type: "docSidebar",
          sidebarId: "apisSidebar",
          position: "left",
          label: "API Reference",
        }, */
        
        // Uncomment when ready to release
        // {
        //   type: "docSidebar",
        //   sidebarId: "roadmapSidebar",
        //   position: "left",
        //   label: "Roadmap and Active developemt areas",
        // }, 
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
    /* announcementBar: {
      id: `beta announcement`,
      // content: `⭐️ If you like Docusaurus, give it a star on <a target="_blank" rel="noopener noreferrer" href="https://github.com/facebook/docusaurus">GitHub</a> and follow us on <a target="_blank" rel="noopener noreferrer" href="https://twitter.com/docusaurus">Twitter </a>`,
      // content: `🎉️ <b><a target="_blank" href="https://docusaurus.io/blog/releases/v">Docusaurus v</a> is out!</b> 🥳️`,
      content: `<b class="announcement-bar-style"> ⚠️ This is a private preview version of KRO. Do not expose to public networks. </b>`,
      backgroundColor: "#ff2617",
      textColor: "white",
    }, */
    prism: {
      theme: prismThemes.oneLight,
      darkTheme: prismThemes.oneDark,
      additionalLanguages: ["bash", "yaml"],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
