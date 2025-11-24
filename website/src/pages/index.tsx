import clsx from "clsx";
import Link from "@docusaurus/Link";
import useDocusaurusContext from "@docusaurus/useDocusaurusContext";
import Layout from "@theme/Layout";
import Heading from "@theme/Heading";
import { useColorMode } from "@docusaurus/theme-common";
import styles from "./index.module.css";

function HomepageHeader() {
  const { siteConfig } = useDocusaurusContext();
  const { colorMode } = useColorMode();

  return (
    <header className={styles.hero}>
      <div className={styles.heroBackground}>
        <div className={styles.gridOverlay}></div>
      </div>

      <div className={styles.heroContent}>
        <h1 className={styles.heroTitle}>
          Kube Resource Orchestrator
        </h1>

        <p className={styles.heroSubtitle}>
          Build declarative, secure, and verifiable Kubernetes abstractions.
        </p>

        <div className={styles.heroButtons}>
          <Link
            className={clsx("button button--lg", styles.primaryButton)}
            to="/docs/overview"
          >
            Documentation
          </Link>
          <Link
            className={clsx("button button--lg", styles.secondaryButton)}
            to="https://github.com/kubernetes-sigs/kro"
          >
            GitHub
          </Link>
        </div>
      </div>
    </header>
  );
}

function TechnicalFeatures() {
  const features = [
    {
      title: "Kubernetes-Native Controller",
      description: "Runs in-cluster as a native controller. Works with any CRD regardless of API group. Continuous reconciliation with automatic drift detection.",
      code: "kubectl get resourcegraphdefinitions"
    },
    {
      title: "One CRD, Simple Schema",
      description: "Define everything in a ResourceGraphDefinition. Use SimpleSchema for concise, readable API schemas.",
      code: "image: string | default=nginx"
    },
    {
      title: "CEL for everything",
      description: "Common Expression Language for all computation. Type-safe, non-Turing-complete, validated at creation time. No arbitrary code execution.",
      code: "${service.status.loadBalancer.ip}"
    },
  ];

  return (
    <section className={styles.technicalFeatures}>
      <div className="container">
        <div className={styles.featuresGrid}>
          {features.map((feature, idx) => (
            <div key={idx} className={styles.featureCard}>
              <div className={styles.featureHeader}>
                <h3>{feature.title}</h3>
              </div>
              <p className={styles.featureDescription}>{feature.description}</p>
              <div className={styles.featureCode}>
                <code>{feature.code}</code>
              </div>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}

export default function Home(): JSX.Element {
  return (
    <Layout
      title="Kube Resource Orchestrator"
      description="Build declarative, secure, and verifiable Kubernetes abstractions"
    >
      <HomepageHeader />
      <main>
        <TechnicalFeatures />
      </main>
    </Layout>
  );
}
