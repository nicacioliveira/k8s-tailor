import * as React from "react";

interface IllustrationProps extends React.SVGAttributes<SVGSVGElement> {
  size?: number;
}

function base(props: IllustrationProps): React.SVGAttributes<SVGSVGElement> {
  const { size = 96, ...rest } = props;
  return {
    width: size,
    height: size,
    viewBox: "0 0 96 96",
    fill: "none",
    xmlns: "http://www.w3.org/2000/svg",
    "aria-hidden": true,
    ...rest,
  };
}

const ACCENT = "var(--primary)";
const DANGER = "var(--destructive)";

export function EmptyInboxIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <rect
        x="14"
        y="36"
        width="68"
        height="42"
        rx="6"
        stroke="currentColor"
        strokeWidth="1.5"
        opacity="0.4"
      />
      <path
        d="M14 50 L40 50 L42 56 L54 56 L56 50 L82 50"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
        opacity="0.6"
      />
      <line
        x1="22"
        y1="22"
        x2="74"
        y2="22"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.2"
      />
      <line
        x1="28"
        y1="14"
        x2="68"
        y2="14"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.15"
      />
      <circle cx="48" cy="50" r="14" fill={ACCENT} opacity="0.08" />
    </svg>
  );
}

export function EmptyListIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <rect
        x="18"
        y="22"
        width="60"
        height="52"
        rx="6"
        stroke="currentColor"
        strokeWidth="1.5"
        opacity="0.4"
      />
      {[34, 44, 54, 64].map((y, i) => (
        <line
          key={y}
          x1="26"
          y1={y}
          x2={i === 1 ? 56 : i === 3 ? 50 : 70}
          y2={y}
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          opacity={i === 0 ? 0.6 : 0.3}
        />
      ))}
      {[34, 44, 54, 64].map((y) => (
        <circle key={y} cx="22" cy={y} r="1.5" fill="currentColor" opacity="0.5" />
      ))}
      <circle cx="74" cy="74" r="9" fill={ACCENT} opacity="0.12" />
      <path
        d="M70 74 L78 74 M74 70 L74 78"
        stroke={ACCENT}
        strokeWidth="1.5"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function EmptySearchIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <circle cx="42" cy="42" r="22" stroke="currentColor" strokeWidth="1.5" opacity="0.4" />
      <line
        x1="58"
        y1="58"
        x2="76"
        y2="76"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        opacity="0.6"
      />
      <circle cx="42" cy="42" r="14" fill={ACCENT} opacity="0.1" />
      <path
        d="M34 38 L34 46 M50 38 L50 46"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.5"
      />
    </svg>
  );
}

export function EmptyChartIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <line x1="18" y1="76" x2="80" y2="76" stroke="currentColor" strokeWidth="1.5" opacity="0.3" />
      <line x1="18" y1="20" x2="18" y2="76" stroke="currentColor" strokeWidth="1.5" opacity="0.3" />
      {[
        [28, 64, 12],
        [42, 52, 24],
        [56, 44, 32],
        [70, 36, 40],
      ].map(([x, y, h]) => (
        <rect key={x} x={x} y={y} width="8" height={h} rx="2" fill="currentColor" opacity="0.4" />
      ))}
      <path
        d="M20 60 Q30 50 40 48 T60 38 T78 26"
        stroke={ACCENT}
        strokeWidth="2"
        strokeLinecap="round"
        fill="none"
      />
      <circle cx="78" cy="26" r="3" fill={ACCENT} />
    </svg>
  );
}

export function ErrorIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <rect
        x="14"
        y="18"
        width="68"
        height="60"
        rx="6"
        stroke="currentColor"
        strokeWidth="1.5"
        opacity="0.4"
      />
      <line x1="14" y1="29" x2="82" y2="29" stroke="currentColor" strokeWidth="1.5" opacity="0.3" />
      {[21, 26, 31].map((cx) => (
        <circle key={cx} cx={cx} cy="23.5" r="1.5" fill="currentColor" opacity="0.5" />
      ))}
      <circle cx="48" cy="54" r="15" fill={ACCENT} opacity="0.08" />
      <path
        d="M48 39 L62 64 L34 64 Z"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
        opacity="0.6"
      />
      <line x1="48" y1="48" x2="48" y2="55" stroke={DANGER} strokeWidth="2" strokeLinecap="round" />
      <circle cx="48" cy="59.5" r="1.25" fill={DANGER} />
    </svg>
  );
}

export function OfflineIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <circle cx="48" cy="54" r="16" fill={ACCENT} opacity="0.08" />
      <path
        d="M20 42 Q48 16 76 42"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.3"
      />
      <path
        d="M29 52 Q48 34 67 52"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.45"
      />
      <path
        d="M38 62 Q48 53 58 62"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.6"
      />
      <circle cx="48" cy="71" r="2.5" fill="currentColor" opacity="0.6" />
      <line x1="24" y1="22" x2="72" y2="78" stroke={ACCENT} strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
}

export function MaintenanceIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <circle cx="38" cy="40" r="18" fill={ACCENT} opacity="0.08" />
      <circle
        cx="38"
        cy="40"
        r="19"
        stroke="currentColor"
        strokeWidth="6"
        strokeDasharray="5 4.95"
        opacity="0.3"
      />
      <circle cx="38" cy="40" r="14" stroke="currentColor" strokeWidth="1.5" opacity="0.6" />
      <circle cx="38" cy="40" r="4.5" stroke="currentColor" strokeWidth="1.5" opacity="0.5" />
      <path
        transform="translate(44 44) scale(1.6)"
        d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"
        stroke={ACCENT}
        strokeWidth="1.1"
        strokeLinejoin="round"
        fill="none"
      />
    </svg>
  );
}

export function LockedIllustration(props: IllustrationProps) {
  return (
    <svg {...base(props)}>
      <circle cx="48" cy="60" r="15" fill={ACCENT} opacity="0.08" />
      <path
        d="M35 44 V34 A13 13 0 0 1 61 34 V44"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.6"
      />
      <rect
        x="26"
        y="44"
        width="44"
        height="34"
        rx="6"
        stroke="currentColor"
        strokeWidth="1.5"
        opacity="0.4"
      />
      <circle cx="48" cy="58" r="3.5" fill={ACCENT} />
      <line x1="48" y1="61" x2="48" y2="68" stroke={ACCENT} strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
}
