import clsx from "clsx";
import Link from "@docusaurus/Link";
import Layout from "@theme/Layout";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import {
  Fragment,
  startTransition,
  useDeferredValue,
  useEffect,
  useRef,
  useState,
  type RefObject,
  type ReactNode,
  type WheelEvent,
} from "react";
import styles from "./roadmap.module.css";

type RoadmapStatus = "Draft" | "In Review" | "Accepted" | "Scheduled" | "Released";
type ReleaseTrackPhase = "released" | "next" | "planned";
type RoadmapSort = "roadmap" | "krep" | "stage";
const ITEMS_PER_PAGE = 10;

type GeneratedRoadmapData = {
  releases: GeneratedReleaseLane[];
  items: GeneratedRoadmapItem[];
};

type GeneratedReleaseLane = {
  release: string;
  url: string;
  phase: ReleaseTrackPhase;
  milestone: {
    number: number;
    title: string;
    state: string;
    url: string;
  };
};

type GeneratedRoadmapItem = {
  id: string;
  title: string;
  summary: string;
  status: string;
  owners: string[];
  release?: string;
  proposalPr?: number;
};

type RoadmapItem = {
  id: string;
  title: string;
  summary: string;
  status: RoadmapStatus;
  owners: string[];
  release?: string;
  proposalPr?: number;
};

type WebpackContextModule = {
  keys(): string[];
  (id: string): unknown;
};

type RequireWithContext = typeof require & {
  context: (
    path: string,
    useSubdirectories?: boolean,
    regExp?: RegExp,
  ) => WebpackContextModule;
};

const generatedRoadmapContext = (require as RequireWithContext).context(
  "../data/generated",
  false,
  /^\.\/krep-roadmap\.json$/,
);

function loadGeneratedRoadmapData(): Partial<GeneratedRoadmapData> {
  try {
    const key = generatedRoadmapContext.keys().find(
      (value) => value === "./krep-roadmap.json",
    );
    if (!key) {
      return {};
    }

    const moduleValue = generatedRoadmapContext(key) as unknown;
    if (typeof moduleValue === "object" && moduleValue !== null) {
      const moduleRecord = moduleValue as {
        default?: Partial<GeneratedRoadmapData>;
      } & Partial<GeneratedRoadmapData>;

      if (moduleRecord.default !== undefined) {
        return moduleRecord.default;
      }

      return moduleRecord;
    }

    return {};
  } catch {
    return {};
  }
}

const generatedRoadmapData = loadGeneratedRoadmapData();
const generatedRoadmapItems = Array.isArray(generatedRoadmapData.items)
  ? generatedRoadmapData.items
  : [];
const generatedReleaseLanes = Array.isArray(generatedRoadmapData.releases)
  ? generatedRoadmapData.releases
  : [];

const roadmapItems: RoadmapItem[] = generatedRoadmapItems.map((item) => ({
  id: item.id,
  title: item.title,
  summary: item.summary,
  status: normalizeRoadmapStatus(item.status),
  owners: item.owners,
  release: item.release || undefined,
  proposalPr: item.proposalPr,
}));
const sortedRoadmapItems = [...roadmapItems].sort(
  (left, right) => roadmapIDNumber(left.id) - roadmapIDNumber(right.id),
);
const releaseMilestoneByName = new Map(
  generatedReleaseLanes.map((milestone) => [milestone.release, milestone]),
);

const statusOrder: RoadmapStatus[] = [
  "Draft",
  "In Review",
  "Accepted",
  "Scheduled",
  "Released",
];
const stageRankByStatus = new Map(statusOrder.map((status, index) => [status, index]));
const unscheduledStatusOrder: RoadmapStatus[] = [
  "In Review",
  "Draft",
  "Accepted",
  "Scheduled",
  "Released",
];
const unscheduledStageRankByStatus = new Map(
  unscheduledStatusOrder.map((status, index) => [status, index]),
);

function normalizeRoadmapStatus(status: string): RoadmapStatus {
  switch (status) {
    case "Draft":
    case "Accepted":
    case "Scheduled":
    case "Released":
    case "In Review":
      return status;
    default:
      return "In Review";
  }
}

function roadmapIDNumber(id: string): number {
  const parsed = Number.parseInt(id, 10);
  return Number.isNaN(parsed) ? 0 : parsed;
}

function sortLabel(sort: RoadmapSort): string {
  switch (sort) {
    case "krep":
      return "KREP number";
    case "stage":
      return "Stage";
    default:
      return "Roadmap order";
  }
}

function sortBucket(item: RoadmapItem): number {
  const releaseLane = item.release ? releaseMilestoneByName.get(item.release) : undefined;
  if (releaseLane?.phase === "next") {
    return 0;
  }
  if (releaseLane?.phase === "planned") {
    return 1;
  }
  if (releaseLane?.phase === "released") {
    return 2;
  }

  return 3;
}

const roadmapReleaseOrder = [
  ...generatedReleaseLanes.filter((lane) => lane.phase === "next").map((lane) => lane.release),
  ...generatedReleaseLanes.filter((lane) => lane.phase === "planned").map((lane) => lane.release),
  ...[...generatedReleaseLanes.filter((lane) => lane.phase === "released")]
    .reverse()
    .map((lane) => lane.release),
];
const roadmapReleaseRankByName = new Map(
  roadmapReleaseOrder.map((release, index) => [release, index]),
);

function compareByStage(left: RoadmapItem, right: RoadmapItem): number {
  const leftStageRank = stageRankByStatus.get(left.status) ?? Number.MAX_SAFE_INTEGER;
  const rightStageRank = stageRankByStatus.get(right.status) ?? Number.MAX_SAFE_INTEGER;
  if (leftStageRank !== rightStageRank) {
    return leftStageRank - rightStageRank;
  }

  return roadmapIDNumber(left.id) - roadmapIDNumber(right.id);
}

function compareByUnscheduledStage(left: RoadmapItem, right: RoadmapItem): number {
  const leftStageRank = unscheduledStageRankByStatus.get(left.status) ?? Number.MAX_SAFE_INTEGER;
  const rightStageRank = unscheduledStageRankByStatus.get(right.status) ?? Number.MAX_SAFE_INTEGER;
  if (leftStageRank !== rightStageRank) {
    return leftStageRank - rightStageRank;
  }

  return roadmapIDNumber(left.id) - roadmapIDNumber(right.id);
}

function compareByRoadmapOrder(left: RoadmapItem, right: RoadmapItem): number {
  const leftBucket = sortBucket(left);
  const rightBucket = sortBucket(right);
  if (leftBucket !== rightBucket) {
    return leftBucket - rightBucket;
  }

  const leftReleaseRank =
    left.release !== undefined
      ? (roadmapReleaseRankByName.get(left.release) ?? Number.MAX_SAFE_INTEGER)
      : Number.MAX_SAFE_INTEGER;
  const rightReleaseRank =
    right.release !== undefined
      ? (roadmapReleaseRankByName.get(right.release) ?? Number.MAX_SAFE_INTEGER)
      : Number.MAX_SAFE_INTEGER;

  if (leftReleaseRank !== rightReleaseRank) {
    return leftReleaseRank - rightReleaseRank;
  }

  if (leftBucket === 3) {
    const stageDiff = compareByUnscheduledStage(left, right);
    if (stageDiff !== 0) {
      return stageDiff;
    }
  }

  return roadmapIDNumber(left.id) - roadmapIDNumber(right.id);
}

function compareRoadmapItems(left: RoadmapItem, right: RoadmapItem, sort: RoadmapSort): number {
  switch (sort) {
    case "krep":
      return roadmapIDNumber(left.id) - roadmapIDNumber(right.id);
    case "stage":
      return compareByStage(left, right);
    default:
      return compareByRoadmapOrder(left, right);
  }
}

const releaseOptions = generatedReleaseLanes.map(({ release }) => release);

const ownerOptions = Array.from(
  new Set(sortedRoadmapItems.flatMap((item) => item.owners)),
).sort((left, right) =>
  left.localeCompare(right, undefined, { sensitivity: "base" }),
);

const stageCounts = new Map(
  statusOrder.map((status) => [
    status,
    sortedRoadmapItems.filter((item) => item.status === status).length,
  ]),
);

const releaseCounts = new Map(
  releaseOptions.map((release) => [
    release,
    sortedRoadmapItems.filter((item) => item.release === release).length,
  ]),
);

const ownerCounts = new Map(
  ownerOptions.map((owner) => [
    owner,
    sortedRoadmapItems.filter((item) => item.owners.includes(owner)).length,
  ]),
);

const releaseLanes = generatedReleaseLanes
  .map((milestone) => ({
    ...milestone,
    items: sortedRoadmapItems.filter((item) => item.release === milestone.release),
  }));

function githubPullRequestLink(pr: number) {
  return `https://github.com/kubernetes-sigs/kro/pull/${pr}`;
}

function githubOwnerLink(owner: string) {
  return `https://github.com/${owner}`;
}

function releaseTrackLabel(phase: ReleaseTrackPhase) {
  switch (phase) {
    case "released":
      return "Released";
    case "next":
      return "Next";
    default:
      return null;
  }
}

function releasePopoverSummary(phase: ReleaseTrackPhase, count: number) {
  if (phase === "released") {
    return `${count} released`;
  }

  if (phase === "next") {
    return `${count} next up`;
  }

  return `${count} planned`;
}

function releasePopoverEmptyState(phase: ReleaseTrackPhase) {
  if (phase === "released") {
    return "No KREPs were mapped to this release.";
  }

  if (phase === "next") {
    return "No KREPs are mapped to the next release yet.";
  }

  return "No KREPs are scheduled for this release yet.";
}

function releasePopoverNote(release: string) {
  if (release === "v1.0.0") {
    return "1.0 is almost here. It's busy eating a few KREPs.";
  }

  return null;
}

function statusClassName(status: RoadmapStatus) {
  switch (status) {
    case "Draft":
      return styles.statusDraft;
    case "In Review":
      return styles.statusInReview;
    case "Accepted":
      return styles.statusAccepted;
    case "Scheduled":
      return styles.statusScheduled;
    case "Released":
      return styles.statusReleased;
  }
}

function StatusBadge({ status }: { status: RoadmapStatus }) {
  return (
    <span className={clsx(styles.statusBadge, statusClassName(status))}>
      {status}
    </span>
  );
}

function InlineOwnerLinks({ owners }: { owners: string[] }) {
  return (
    <div className={styles.inlineLinkList}>
      {owners.map((owner, index) => (
        <span key={owner}>
          {index > 0 ? <span className={styles.separator}>, </span> : null}
          <Link className={styles.resourceLink} href={githubOwnerLink(owner)}>
            @{owner}
          </Link>
        </span>
      ))}
    </div>
  );
}

function FilterDropdown({
  label,
  isOpen,
  menuLabel,
  menuRef,
  onToggle,
  selectedContent,
  children,
}: {
  label: string;
  isOpen: boolean;
  menuLabel: string;
  menuRef: RefObject<HTMLDivElement | null>;
  onToggle: () => void;
  selectedContent: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className={styles.filterGroup}>
      <span className={styles.filterGroupLabel}>{label}</span>
      <div
        ref={menuRef}
        className={clsx(styles.filterMenuShell, isOpen && styles.filterMenuShellOpen)}
      >
        <button
          type="button"
          className={clsx(styles.filterTrigger, isOpen && styles.filterTriggerOpen)}
          aria-haspopup="listbox"
          aria-expanded={isOpen}
          aria-label={`${label} filter`}
          onClick={onToggle}
        >
          <span className={styles.filterTriggerContent}>{selectedContent}</span>
          <span
            className={clsx(
              styles.filterTriggerChevron,
              isOpen && styles.filterTriggerChevronOpen,
            )}
            aria-hidden="true"
          />
        </button>

        {isOpen ? (
          <div className={styles.filterMenu} role="listbox" aria-label={menuLabel}>
            {children}
          </div>
        ) : null}
      </div>
    </div>
  );
}

function KrepCard({ item }: { item: RoadmapItem }) {
  const cardTitle = `KREP-${item.id}: ${item.title}`;
  const releaseMilestone = item.release ? releaseMilestoneByName.get(item.release) : undefined;

  return (
    <article className={styles.card}>
      <div className={styles.cardHeader}>
        <h3 className={styles.cardTitle}>
          {item.proposalPr ? (
            <Link
              className={styles.cardTitleLink}
              href={githubPullRequestLink(item.proposalPr)}
            >
              {cardTitle}
            </Link>
          ) : (
            cardTitle
          )}
        </h3>
        <StatusBadge status={item.status} />
      </div>

      <div className={styles.cardSummary}>
        <ReactMarkdown remarkPlugins={[remarkGfm]}>{item.summary}</ReactMarkdown>
      </div>

      <dl className={styles.cardMeta}>
        <div className={styles.cardMetaBlock}>
          <dt>Release</dt>
          <dd>
            {item.release ? (
              releaseMilestone ? (
                <Link
                  className={clsx(styles.releaseBadge, styles.releaseBadgeLink)}
                  href={releaseMilestone.url}
                >
                  {item.release}
                </Link>
              ) : (
                <span className={styles.releaseBadge}>{item.release}</span>
              )
            ) : (
              <span className={styles.emptyValue}>Not yet scheduled</span>
            )}
          </dd>
        </div>
        <div className={styles.cardMetaBlock}>
          <dt>Authors</dt>
          <dd>
            <InlineOwnerLinks owners={item.owners} />
          </dd>
        </div>
        <div className={styles.cardMetaBlock}>
          <dt>Proposal PR</dt>
          <dd>
            {item.proposalPr ? (
              <Link className={styles.resourceLink} href={githubPullRequestLink(item.proposalPr)}>
                #{item.proposalPr}
              </Link>
            ) : (
              <span className={styles.emptyValue}>Not linked</span>
            )}
          </dd>
        </div>
      </dl>
    </article>
  );
}

export default function RoadmapPage(): JSX.Element {
  const [keyword, setKeyword] = useState("");
  const [selectedRelease, setSelectedRelease] = useState("all");
  const [selectedStage, setSelectedStage] = useState("all");
  const [selectedOwner, setSelectedOwner] = useState("all");
  const [selectedSort, setSelectedSort] = useState<RoadmapSort>("roadmap");
  const [selectedPage, setSelectedPage] = useState(1);
  const [openFilterMenu, setOpenFilterMenu] = useState<
    "release" | "stage" | "owner" | "sort" | null
  >(null);
  const [resultsMinHeight, setResultsMinHeight] = useState<number | null>(null);
  const resultsRef = useRef<HTMLDivElement>(null);
  const releaseFilterMenuRef = useRef<HTMLDivElement>(null);
  const stageFilterMenuRef = useRef<HTMLDivElement>(null);
  const ownerFilterMenuRef = useRef<HTMLDivElement>(null);
  const sortFilterMenuRef = useRef<HTMLDivElement>(null);
  const releaseRailShellRef = useRef<HTMLDivElement>(null);
  const releaseRailViewportRef = useRef<HTMLDivElement>(null);
  const releaseRailTrackRef = useRef<HTMLDivElement>(null);
  const releaseRailPreviewRef = useRef<HTMLDivElement>(null);
  const [canScrollReleaseLeft, setCanScrollReleaseLeft] = useState(false);
  const [canScrollReleaseRight, setCanScrollReleaseRight] = useState(false);
  const [activeReleaseNav, setActiveReleaseNav] = useState<"left" | "right" | null>(null);
  const releaseNavResetRef = useRef<number | null>(null);
  const hasCenteredNextReleaseRef = useRef(false);
  const defaultPreviewRelease =
    releaseLanes.find((lane) => lane.phase === "next")?.release ?? releaseLanes[0]?.release ?? null;
  const [activeReleasePreview, setActiveReleasePreview] = useState<string | null>(null);
  const [activeReleasePreviewLeft, setActiveReleasePreviewLeft] = useState<number | null>(null);

  const deferredKeyword = useDeferredValue(keyword);
  const normalizedKeyword = deferredKeyword.trim().toLowerCase();

  const filteredItems = sortedRoadmapItems.filter((item) => {
    if (selectedRelease !== "all" && item.release !== selectedRelease) {
      return false;
    }

    if (selectedStage !== "all" && item.status !== selectedStage) {
      return false;
    }

    if (selectedOwner !== "all" && !item.owners.includes(selectedOwner)) {
      return false;
    }

    if (!normalizedKeyword) {
      return true;
    }

    const searchValue = [
      `krep-${item.id}`,
      `krep-${roadmapIDNumber(item.id)}`,
      item.title,
      item.summary,
      item.status,
      item.owners.join(" "),
      item.release ?? "",
      item.proposalPr ? `#${item.proposalPr}` : "",
    ]
      .join(" ")
      .toLowerCase();

    return searchValue.includes(normalizedKeyword);
  });
  const orderedFilteredItems = [...filteredItems].sort((left, right) =>
    compareRoadmapItems(left, right, selectedSort),
  );

  const hasActiveFilters =
    keyword.length > 0 ||
    selectedRelease !== "all" ||
    selectedStage !== "all" ||
    selectedOwner !== "all";

  const pageCount = Math.max(1, Math.ceil(orderedFilteredItems.length / ITEMS_PER_PAGE));
  const currentPage = Math.min(selectedPage, pageCount);
  const paginatedItems = orderedFilteredItems.slice(
    (currentPage - 1) * ITEMS_PER_PAGE,
    currentPage * ITEMS_PER_PAGE,
  );
  const paginationPages = Array.from({ length: pageCount }, (_, index) => index + 1);
  const resultsSignature = paginatedItems.map((item) => item.id).join(",");

  function preserveResultsHeight() {
    if (!resultsRef.current) {
      return;
    }

    setResultsMinHeight(Math.ceil(resultsRef.current.getBoundingClientRect().height));
  }

  function updateReleasePreviewPosition(release: string | null, stopElement?: HTMLElement | null) {
    if (!release) {
      return;
    }

    const shell = releaseRailShellRef.current;
    if (!shell) {
      return;
    }

    const targetStop =
      stopElement ??
      Array.from(shell.querySelectorAll<HTMLElement>("[data-release-stop]")).find(
        (element) => element.dataset.releaseName === release,
      );

    if (!targetStop) {
      return;
    }

    const shellRect = shell.getBoundingClientRect();
    const stopRect = targetStop.getBoundingClientRect();
    const previewWidth = releaseRailPreviewRef.current?.offsetWidth ?? 416;
    const halfPreviewWidth = previewWidth / 2;
    const edgeInset = 12;
    const stopCenter = stopRect.left - shellRect.left + stopRect.width / 2;
    const clampedCenter = Math.max(
      edgeInset + halfPreviewWidth,
      Math.min(shellRect.width - edgeInset - halfPreviewWidth, stopCenter),
    );

    setActiveReleasePreviewLeft(clampedCenter);
  }

  function activateReleasePreview(release: string, stopElement?: HTMLElement | null) {
    setActiveReleasePreview(release);
    updateReleasePreviewPosition(release, stopElement);
  }

  function resetReleasePreview() {
    setActiveReleasePreview(null);
    setActiveReleasePreviewLeft(null);
  }

  useEffect(() => {
    if (!resultsRef.current || resultsMinHeight === null) {
      return;
    }

    const nextHeight = Math.ceil(resultsRef.current.getBoundingClientRect().height);
    const frameId = window.requestAnimationFrame(() => {
      setResultsMinHeight(nextHeight);
    });
    const timeoutId = window.setTimeout(() => {
      setResultsMinHeight(null);
    }, 220);

    return () => {
      window.cancelAnimationFrame(frameId);
      window.clearTimeout(timeoutId);
    };
  }, [resultsMinHeight, resultsSignature, currentPage]);

  useEffect(() => {
    function updateReleaseRailControls() {
      const viewport = releaseRailViewportRef.current;
      if (!viewport) {
        return;
      }

      const maxScrollLeft = viewport.scrollWidth - viewport.clientWidth;
      setCanScrollReleaseLeft(viewport.scrollLeft > 2);
      setCanScrollReleaseRight(maxScrollLeft - viewport.scrollLeft > 2);

      if (activeReleasePreview) {
        updateReleasePreviewPosition(activeReleasePreview);
      }
    }

    updateReleaseRailControls();

    const viewport = releaseRailViewportRef.current;
    viewport?.addEventListener("scroll", updateReleaseRailControls, { passive: true });
    window.addEventListener("resize", updateReleaseRailControls);

    return () => {
      viewport?.removeEventListener("scroll", updateReleaseRailControls);
      window.removeEventListener("resize", updateReleaseRailControls);
    };
  }, [activeReleasePreview, releaseLanes.length]);

  useEffect(() => {
    if (hasCenteredNextReleaseRef.current) {
      return;
    }

    const viewport = releaseRailViewportRef.current;
    const track = releaseRailTrackRef.current;
    const nextStop = track?.querySelector<HTMLElement>('[data-release-phase="next"]');

    if (!viewport || !nextStop) {
      return;
    }

    const frameId = window.requestAnimationFrame(() => {
      const maxScrollLeft = viewport.scrollWidth - viewport.clientWidth;
      const nextScrollLeft =
        nextStop.offsetLeft - (viewport.clientWidth - nextStop.offsetWidth) / 2;

      viewport.scrollTo({
        left: Math.max(0, Math.min(nextScrollLeft, maxScrollLeft)),
        behavior: "auto",
      });

      hasCenteredNextReleaseRef.current = true;
      viewport.dispatchEvent(new Event("scroll"));
    });

    return () => {
      window.cancelAnimationFrame(frameId);
    };
  }, [defaultPreviewRelease]);

  useEffect(() => {
    return () => {
      if (releaseNavResetRef.current) {
        window.clearTimeout(releaseNavResetRef.current);
      }
    };
  }, []);

  useEffect(() => {
    if (!activeReleasePreview || releaseLanes.some((lane) => lane.release === activeReleasePreview)) {
      return;
    }

    setActiveReleasePreview(null);
    setActiveReleasePreviewLeft(null);
  }, [activeReleasePreview]);

  useEffect(() => {
    if (!activeReleasePreview) {
      return;
    }

    const frameId = window.requestAnimationFrame(() => {
      updateReleasePreviewPosition(activeReleasePreview);
    });

    return () => {
      window.cancelAnimationFrame(frameId);
    };
  }, [activeReleasePreview]);

  const activePreviewLane = releaseLanes.find((lane) => lane.release === activeReleasePreview);

  function scrollReleaseRail(direction: -1 | 1) {
    const viewport = releaseRailViewportRef.current;
    const track = releaseRailTrackRef.current;
    const firstStop = track?.querySelector<HTMLElement>("[data-release-stop]");

    if (!viewport || !track || !firstStop) {
      return;
    }

    const trackStyles = window.getComputedStyle(track);
    const step =
      firstStop.getBoundingClientRect().width +
      parseFloat(trackStyles.columnGap || trackStyles.gap || "0");

    viewport.scrollBy({
      left: direction * step,
      behavior: "smooth",
    });

    setActiveReleaseNav(direction === -1 ? "left" : "right");
    if (releaseNavResetRef.current) {
      window.clearTimeout(releaseNavResetRef.current);
    }
    releaseNavResetRef.current = window.setTimeout(() => {
      setActiveReleaseNav(null);
    }, 240);
  }

  function handleReleaseRailWheel(event: WheelEvent<HTMLDivElement>) {
    const viewport = releaseRailViewportRef.current;
    if (!viewport) {
      return;
    }

    if (Math.abs(event.deltaX) <= Math.abs(event.deltaY)) {
      return;
    }

    event.preventDefault();
    viewport.scrollBy({
      left: event.deltaX,
      behavior: "auto",
    });
  }

  function closeFilterMenu() {
    setOpenFilterMenu(null);
  }

  function applyReleaseFilter(value: string) {
    startTransition(() => {
      preserveResultsHeight();
      setSelectedRelease(value);
      setSelectedPage(1);
      closeFilterMenu();
    });
  }

  function applyStageFilter(value: RoadmapStatus | "all") {
    startTransition(() => {
      preserveResultsHeight();
      setSelectedStage(value);
      setSelectedPage(1);
      closeFilterMenu();
    });
  }

  function applyOwnerFilter(value: string) {
    startTransition(() => {
      preserveResultsHeight();
      setSelectedOwner(value);
      setSelectedPage(1);
      closeFilterMenu();
    });
  }

  function applySort(value: RoadmapSort) {
    startTransition(() => {
      preserveResultsHeight();
      setSelectedSort(value);
      setSelectedPage(1);
      closeFilterMenu();
    });
  }

  useEffect(() => {
    if (!openFilterMenu) {
      return;
    }

    const activeMenuRef =
      openFilterMenu === "release"
        ? releaseFilterMenuRef
        : openFilterMenu === "stage"
          ? stageFilterMenuRef
          : openFilterMenu === "owner"
            ? ownerFilterMenuRef
            : sortFilterMenuRef;

    function handlePointerDown(event: MouseEvent) {
      const target = event.target;
      if (!(target instanceof Node)) {
        return;
      }

      if (activeMenuRef.current?.contains(target)) {
        return;
      }

      closeFilterMenu();
    }

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") {
        closeFilterMenu();
      }
    }

    document.addEventListener("mousedown", handlePointerDown);
    document.addEventListener("keydown", handleKeyDown);

    return () => {
      document.removeEventListener("mousedown", handlePointerDown);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [openFilterMenu]);

  return (
    <Layout
      title="Roadmap"
      description="Preview of the kro roadmap page for KREPs and release planning"
    >
      <main className={styles.page}>
        <div className="container margin-vert--lg">
          <section className={styles.intro}>
            <h1>Roadmap</h1>
            <p className={styles.introLead}>
              Track every KREP from review to release, see what lands in each milestone,
              and jump straight to its proposal, issues, and authors.
            </p>
            <p className={styles.introSupport}>
              The roadmap brings proposal status and release planning into one place so
              contributors can follow active work, understand upcoming milestones, and
              quickly find the GitHub threads behind each item.
            </p>
          </section>

          <section className={styles.section}>
            <div className={styles.sectionHeader}>
              <div>
                <h2>Release lanes</h2>
                <p>
                  Each release can contain multiple KREPs. Hover a release to preview them,
                  and click it to open the configured GitHub milestone.
                </p>
              </div>
            </div>

            <div
              ref={releaseRailShellRef}
              className={clsx(
                styles.releaseRailShell,
              )}
              onMouseLeave={resetReleasePreview}
              onBlurCapture={(event) => {
                const nextFocusTarget = event.relatedTarget;
                if (nextFocusTarget instanceof Node && event.currentTarget.contains(nextFocusTarget)) {
                  return;
                }
                resetReleasePreview();
              }}
            >
              {activePreviewLane ? (
                <div
                  ref={releaseRailPreviewRef}
                  className={styles.releaseRailPreview}
                  style={
                    activeReleasePreviewLeft !== null
                      ? { left: `${activeReleasePreviewLeft}px` }
                      : undefined
                  }
                >
                  <div className={styles.releasePopoverHeader}>
                    <span className={styles.releaseBadge}>{activePreviewLane.release}</span>
                    <span className={styles.releasePopoverCount}>
                      {releasePopoverSummary(
                        activePreviewLane.phase,
                        activePreviewLane.items.length,
                      )}
                    </span>
                  </div>

                  {releasePopoverNote(activePreviewLane.release) ? (
                    <div className={styles.releasePopoverNote}>
                      {releasePopoverNote(activePreviewLane.release)}
                    </div>
                  ) : null}

                  {activePreviewLane.items.length > 0 ? (
                    <div className={styles.releasePopoverList}>
                      {activePreviewLane.items.map((item) => (
                        <div
                          key={`${activePreviewLane.release}-${item.id}`}
                          className={styles.releasePopoverItem}
                        >
                          {item.proposalPr ? (
                            <Link
                              className={styles.resourceLink}
                              href={githubPullRequestLink(item.proposalPr)}
                            >
                              KREP-{item.id}: {item.title}
                            </Link>
                          ) : (
                            <span className={styles.resourceText}>
                              KREP-{item.id}: {item.title}
                            </span>
                          )}
                        </div>
                      ))}
                    </div>
                  ) : (
                    <div className={styles.releasePopoverEmpty}>
                      {releasePopoverEmptyState(activePreviewLane.phase)}
                    </div>
                  )}
                </div>
              ) : null}

              <div className={styles.releaseRailLine} aria-hidden="true" />

              <button
                type="button"
                className={clsx(
                  styles.releaseRailNav,
                  styles.releaseRailNavLeft,
                  activeReleaseNav === "left" && styles.releaseRailNavActive,
                )}
                aria-label="Show earlier releases"
                disabled={!canScrollReleaseLeft}
                onClick={() => scrollReleaseRail(-1)}
              >
                ‹
              </button>

              <div
                ref={releaseRailViewportRef}
                className={styles.releaseRailViewport}
                onWheel={handleReleaseRailWheel}
              >
                <div ref={releaseRailTrackRef} className={styles.releaseRailTrack}>
                  {releaseLanes.map((lane, index) => (
                    <Fragment key={lane.release}>
                      {index === releaseLanes.length - 1 ? (
                        <div
                          className={clsx(styles.releaseStop, styles.releaseStopPlaceholder)}
                          aria-hidden="true"
                        >
                          <div className={styles.releaseStopButtonPlaceholder}>
                            <span
                              className={clsx(
                                styles.releaseStopDot,
                                styles.releaseStopDotPlaceholder,
                              )}
                              aria-hidden="true"
                            />
                            <span className={styles.releaseStopPlaceholderLabel}>...</span>
                          </div>
                        </div>
                      ) : null}

                      <div
                        key={lane.release}
                        className={styles.releaseStop}
                        data-release-stop
                        data-release-phase={lane.phase}
                        data-release-name={lane.release}
                        onMouseEnter={(event) =>
                          activateReleasePreview(lane.release, event.currentTarget)
                        }
                        onFocusCapture={(event) =>
                          activateReleasePreview(
                            lane.release,
                            event.currentTarget as HTMLDivElement,
                          )
                        }
                      >
                        <Link
                          className={clsx(
                            styles.releaseStopButton,
                            lane.phase === "released" && styles.releaseStopButtonReleased,
                            lane.phase === "next" && styles.releaseStopButtonNext,
                          )}
                          href={lane.url}
                        >
                          <span className={styles.releaseStopDot} aria-hidden="true" />
                          <span className={styles.releaseStopLabel}>{lane.release}</span>
                          <span className={styles.releaseStopMeta}>
                            {releaseTrackLabel(lane.phase) ? (
                              <span
                                className={clsx(
                                  styles.releaseStopPhase,
                                  lane.phase === "released" && styles.releaseStopPhaseReleased,
                                  lane.phase === "next" && styles.releaseStopPhaseNext,
                                )}
                              >
                                {releaseTrackLabel(lane.phase)}
                              </span>
                            ) : null}
                            <span className={styles.releaseStopCount}>
                              {lane.items.length} KREP{lane.items.length === 1 ? "" : "s"}
                            </span>
                          </span>
                        </Link>
                      </div>
                    </Fragment>
                  ))}
                </div>
              </div>

              <button
                type="button"
                className={clsx(
                  styles.releaseRailNav,
                  styles.releaseRailNavRight,
                  activeReleaseNav === "right" && styles.releaseRailNavActive,
                )}
                aria-label="Show later releases"
                disabled={!canScrollReleaseRight}
                onClick={() => scrollReleaseRail(1)}
              >
                ›
              </button>
            </div>
          </section>

          <section className={styles.section}>
            <div className={styles.roadmapLead}>
              <div className={clsx(styles.sectionHeader, styles.roadmapHeader)}>
                <div>
                  <h2>KREP roadmap</h2>
                  <p>Search, filter, and sort by keyword, release, stage, or author.</p>
                </div>
              </div>

              <div className={styles.filterPanel}>
                <div className={styles.filterToolbar}>
                  <label className={styles.searchField}>
                    <span>Search</span>
                    <input
                      className={styles.input}
                      type="search"
                      value={keyword}
                      onChange={(event) =>
                        startTransition(() => {
                          preserveResultsHeight();
                          setKeyword(event.target.value);
                          setSelectedPage(1);
                        })
                      }
                      placeholder="Search KREPs, PRs, issues, releases, or authors"
                    />
                  </label>

                  <div className={styles.filterActions}>
                    <span className={styles.resultCount}>
                      {orderedFilteredItems.length} result{orderedFilteredItems.length === 1 ? "" : "s"}
                    </span>
                    {hasActiveFilters ? (
                      <button
                        type="button"
                        className={styles.resetButton}
                        onClick={() =>
                          startTransition(() => {
                            preserveResultsHeight();
                            setKeyword("");
                            setSelectedRelease("all");
                            setSelectedStage("all");
                            setSelectedOwner("all");
                            closeFilterMenu();
                          })
                        }
                      >
                        Clear filters
                      </button>
                    ) : null}
                  </div>
                </div>

                <div className={styles.filterGroups}>
                  <FilterDropdown
                    label="Release"
                    isOpen={openFilterMenu === "release"}
                    menuLabel="Release filter options"
                    menuRef={releaseFilterMenuRef}
                    onToggle={() =>
                      setOpenFilterMenu((current) => (current === "release" ? null : "release"))
                    }
                    selectedContent={
                      selectedRelease === "all" ? (
                        <span className={styles.filterTriggerPlaceholder}>All releases</span>
                      ) : (
                        <span className={clsx(styles.releaseBadge, styles.filterTriggerValueToken)}>
                          {selectedRelease}
                        </span>
                      )
                    }
                  >
                    <button
                      type="button"
                      className={clsx(
                        styles.filterOption,
                        selectedRelease === "all" && styles.filterOptionActive,
                      )}
                      onClick={() => applyReleaseFilter("all")}
                    >
                      <span className={styles.filterOptionBody}>
                        <span className={styles.filterOptionLabel}>All releases</span>
                        <span className={styles.filterOptionMeta}>Show every version on the roadmap</span>
                      </span>
                      {selectedRelease === "all" ? (
                        <span className={styles.filterOptionCheck} aria-hidden="true">
                          ✓
                        </span>
                      ) : null}
                    </button>

                    {releaseOptions.map((release) => (
                      <button
                        key={release}
                        type="button"
                        className={clsx(
                          styles.filterOption,
                          selectedRelease === release && styles.filterOptionActive,
                        )}
                        onClick={() => applyReleaseFilter(release)}
                      >
                        <span className={styles.filterOptionBody}>
                          <span className={clsx(styles.releaseBadge, styles.filterOptionRelease)}>
                            {release}
                          </span>
                          <span className={styles.filterOptionMeta}>
                            {releaseCounts.get(release)} KREP
                            {releaseCounts.get(release) === 1 ? "" : "s"}
                          </span>
                        </span>
                        {selectedRelease === release ? (
                          <span className={styles.filterOptionCheck} aria-hidden="true">
                            ✓
                          </span>
                        ) : null}
                      </button>
                    ))}
                  </FilterDropdown>

                  <FilterDropdown
                    label="Stage"
                    isOpen={openFilterMenu === "stage"}
                    menuLabel="Stage filter options"
                    menuRef={stageFilterMenuRef}
                    onToggle={() =>
                      setOpenFilterMenu((current) => (current === "stage" ? null : "stage"))
                    }
                    selectedContent={
                      selectedStage === "all" ? (
                        <span className={styles.filterTriggerPlaceholder}>All stages</span>
                      ) : (
                        <StatusBadge status={selectedStage as RoadmapStatus} />
                      )
                    }
                  >
                    <button
                      type="button"
                      className={clsx(
                        styles.filterOption,
                        selectedStage === "all" && styles.filterOptionActive,
                      )}
                      onClick={() => applyStageFilter("all")}
                    >
                      <span className={styles.filterOptionBody}>
                        <span className={styles.filterOptionLabel}>All stages</span>
                        <span className={styles.filterOptionMeta}>Show the full proposal pipeline</span>
                      </span>
                      {selectedStage === "all" ? (
                        <span className={styles.filterOptionCheck} aria-hidden="true">
                          ✓
                        </span>
                      ) : null}
                    </button>

                    {statusOrder.map((status) => (
                      <button
                        key={status}
                        type="button"
                        className={clsx(
                          styles.filterOption,
                          selectedStage === status && styles.filterOptionActive,
                        )}
                        onClick={() => applyStageFilter(status)}
                      >
                        <span className={styles.filterOptionBody}>
                          <span className={styles.filterOptionStage}>
                            <StatusBadge status={status} />
                          </span>
                          <span className={styles.filterOptionMeta}>
                            {stageCounts.get(status)} KREP{stageCounts.get(status) === 1 ? "" : "s"}
                          </span>
                        </span>
                        {selectedStage === status ? (
                          <span className={styles.filterOptionCheck} aria-hidden="true">
                            ✓
                          </span>
                        ) : null}
                      </button>
                    ))}
                  </FilterDropdown>

                  <FilterDropdown
                    label="Author"
                    isOpen={openFilterMenu === "owner"}
                    menuLabel="Author filter options"
                    menuRef={ownerFilterMenuRef}
                    onToggle={() =>
                      setOpenFilterMenu((current) => (current === "owner" ? null : "owner"))
                    }
                    selectedContent={
                      selectedOwner === "all" ? (
                        <span className={styles.filterTriggerPlaceholder}>All authors</span>
                      ) : (
                        <span className={styles.filterTriggerOwner}>@{selectedOwner}</span>
                      )
                    }
                  >
                    <button
                      type="button"
                      className={clsx(
                        styles.filterOption,
                        selectedOwner === "all" && styles.filterOptionActive,
                      )}
                      onClick={() => applyOwnerFilter("all")}
                    >
                      <span className={styles.filterOptionBody}>
                        <span className={styles.filterOptionLabel}>All authors</span>
                        <span className={styles.filterOptionMeta}>Show every KREP author</span>
                      </span>
                      {selectedOwner === "all" ? (
                        <span className={styles.filterOptionCheck} aria-hidden="true">
                          ✓
                        </span>
                      ) : null}
                    </button>

                    {ownerOptions.map((owner) => (
                      <button
                        key={owner}
                        type="button"
                        className={clsx(
                          styles.filterOption,
                          selectedOwner === owner && styles.filterOptionActive,
                        )}
                        onClick={() => applyOwnerFilter(owner)}
                      >
                        <span className={styles.filterOptionBody}>
                          <span className={styles.filterOptionOwner}>@{owner}</span>
                          <span className={styles.filterOptionMeta}>
                            {ownerCounts.get(owner)} KREP{ownerCounts.get(owner) === 1 ? "" : "s"}
                          </span>
                        </span>
                        {selectedOwner === owner ? (
                          <span className={styles.filterOptionCheck} aria-hidden="true">
                            ✓
                          </span>
                        ) : null}
                      </button>
                    ))}
                  </FilterDropdown>

                  <FilterDropdown
                    label="Sort by"
                    isOpen={openFilterMenu === "sort"}
                    menuLabel="Sort order options"
                    menuRef={sortFilterMenuRef}
                    onToggle={() =>
                      setOpenFilterMenu((current) => (current === "sort" ? null : "sort"))
                    }
                    selectedContent={
                      <span className={styles.filterTriggerPlaceholder}>
                        {sortLabel(selectedSort)}
                      </span>
                    }
                  >
                    <button
                      type="button"
                      className={clsx(
                        styles.filterOption,
                        selectedSort === "roadmap" && styles.filterOptionActive,
                      )}
                      onClick={() => applySort("roadmap")}
                    >
                      <span className={styles.filterOptionBody}>
                        <span className={styles.filterOptionLabel}>Roadmap order</span>
                        <span className={styles.filterOptionMeta}>
                          Next release, future releases, older releases, then unscheduled work
                        </span>
                      </span>
                      {selectedSort === "roadmap" ? (
                        <span className={styles.filterOptionCheck} aria-hidden="true">
                          ✓
                        </span>
                      ) : null}
                    </button>

                    <button
                      type="button"
                      className={clsx(
                        styles.filterOption,
                        selectedSort === "krep" && styles.filterOptionActive,
                      )}
                      onClick={() => applySort("krep")}
                    >
                      <span className={styles.filterOptionBody}>
                        <span className={styles.filterOptionLabel}>KREP number</span>
                        <span className={styles.filterOptionMeta}>
                          Show proposals in numerical KREP order
                        </span>
                      </span>
                      {selectedSort === "krep" ? (
                        <span className={styles.filterOptionCheck} aria-hidden="true">
                          ✓
                        </span>
                      ) : null}
                    </button>

                    <button
                      type="button"
                      className={clsx(
                        styles.filterOption,
                        selectedSort === "stage" && styles.filterOptionActive,
                      )}
                      onClick={() => applySort("stage")}
                    >
                      <span className={styles.filterOptionBody}>
                        <span className={styles.filterOptionLabel}>Stage</span>
                        <span className={styles.filterOptionMeta}>
                          Group proposals by review stage
                        </span>
                      </span>
                      {selectedSort === "stage" ? (
                        <span className={styles.filterOptionCheck} aria-hidden="true">
                          ✓
                        </span>
                      ) : null}
                    </button>
                  </FilterDropdown>
                </div>
              </div>
            </div>

            <div
              ref={resultsRef}
              className={styles.resultsShell}
              style={resultsMinHeight ? { minHeight: `${resultsMinHeight}px` } : undefined}
            >
              {orderedFilteredItems.length > 0 ? (
                <>
                  <div className={styles.cardList}>
                    {paginatedItems.map((item) => (
                      <KrepCard key={item.id} item={item} />
                    ))}
                  </div>

                  {pageCount > 1 ? (
                    <nav className={styles.pagination} aria-label="KREP roadmap pagination">
                      {paginationPages.map((page) => (
                        <button
                          key={page}
                          type="button"
                          className={clsx(
                            styles.paginationButton,
                            currentPage === page && styles.paginationButtonActive,
                          )}
                          onClick={() =>
                            startTransition(() => {
                              preserveResultsHeight();
                              setSelectedPage(page);
                            })
                          }
                        >
                          {page}
                        </button>
                      ))}
                    </nav>
                  ) : null}
                </>
              ) : (
                <div className={styles.emptyState}>
                  No KREPs match the current filters.
                </div>
              )}
            </div>
          </section>
        </div>
      </main>
    </Layout>
  );
}
