import clsx from "clsx";
import Heading from "@theme/Heading";
import styles from "./styles.module.css";
import { useColorMode } from "@docusaurus/theme-common";

type FeatureItem = {
  title: string;
  scale: number;
  id: string;
  fill: string;
  Svg: React.ComponentType<React.ComponentProps<"svg">>;
  description: JSX.Element;
};

const FeatureList: FeatureItem[] = [
  {
    title: "Powerful Abstractions",
    id: "benzene-ring-svgrepo-com",
    fill: "white",
    scale: 0.7,
    Svg: require("@site/static/img/resources.svg").default,
    description: (
      <>
        Create powerful abstractions that encapsulate complex Kubernetes
        resources, enabling easier management and reuse across your
        organization.
      </>
    ),
  },
  {
    title: "Effortless Orchestration",
    Svg: require("@site/static/img/qrcode.svg").default,
    id: "qr-code-svgrepo-com",
    fill: "a2",
    scale: 0.6,
    description: (
      <>
        kro streamlines Kubernetes complexity, allowing you to manage resources
        intuitively and focus on developing your application, not wrestling with
        YAML files.
      </>
    ),
  },
  {
    title: "Seamless Scalability",
    scale: 0.6,
    id: "scale-svgrepo-com",
    fill: "white",
    Svg: require("@site/static/img/expand-arrows.svg").default,
    description: (
      <>
        kro effortlessly scales your resource management from simple deployments
        to complex, multi-service architectures.
      </>
    ),
  },
];

function Feature({ scale, fill, id, title, Svg, description }: FeatureItem) {
  const { colorMode } = useColorMode();
  if (colorMode === "light") {
    fill = "var(--main-color)";
  } else if (colorMode === "dark") {
    fill = "var(--main-color-dark)";
  }

  return (
    <div className={clsx("col col--4")}>
      <div className="text--center">
        <Svg
          transform={"scale(" + scale + ")"}
          fill={fill}
          id={id}
          className={styles.featureSvg}
          role="img"
        />
      </div>
      <div className="text--center padding-horiz--md">
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures(): JSX.Element {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
