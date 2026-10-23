import * as React from "react";

import { cn } from "./cn";

type IconComponent = React.ComponentType<{
  className?: string;
  "aria-hidden"?: boolean | "true" | "false";
}>;
type IconProp = IconComponent | React.ReactElement;

function renderIcon(
  icon: IconProp | null | undefined | false,
  className?: string,
): React.ReactNode {
  if (!icon) return null;
  if (React.isValidElement<{ className?: string }>(icon)) {
    return React.cloneElement(
      icon as React.ReactElement<{ className?: string; "aria-hidden"?: boolean }>,
      {
        "aria-hidden": true,
        className: cn(className, icon.props.className),
      },
    );
  }
  return React.createElement(icon as IconComponent, { "aria-hidden": true, className });
}

export { renderIcon };
export type { IconProp };
