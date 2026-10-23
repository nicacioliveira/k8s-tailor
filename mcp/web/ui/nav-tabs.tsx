import * as React from "react";

import { cn } from "./lib/cn";
import { type IconProp, renderIcon } from "./lib/icon";

type NavTabsIcon = IconProp;

interface NavTabsItem {
  id: string;
  label: string;
  href: string;
  icon?: NavTabsIcon;
  count?: { value: number; label: string };
}

interface NavTabsLinkProps {
  href: string;
  className: string;
  "aria-current"?: "page";
  "data-active"?: "true";
  children: React.ReactNode;
}

interface NavTabsProps extends Omit<React.ComponentProps<"nav">, "children"> {
  items: readonly NavTabsItem[];
  activeId?: string;
  renderLink?: (props: NavTabsLinkProps, item: NavTabsItem) => React.ReactNode;
  "aria-label"?: string;
}

const defaultRenderLink = (props: NavTabsLinkProps) => <a {...props} />;

function NavTabs({
  items,
  activeId,
  renderLink = defaultRenderLink,
  className,
  "aria-label": ariaLabel = "Sections",
  ...props
}: NavTabsProps) {
  const listRef = React.useRef<HTMLUListElement>(null);

  React.useLayoutEffect(() => {
    const list = listRef.current;
    const active = list?.querySelector<HTMLElement>("[aria-current=page]");
    if (!list || !active || list.scrollWidth <= list.clientWidth) return;
    const left =
      active.getBoundingClientRect().left - list.getBoundingClientRect().left + list.scrollLeft;
    if (left < list.scrollLeft || left + active.offsetWidth > list.scrollLeft + list.clientWidth) {
      list.scrollLeft = left - 16;
    }
  }, [activeId]);

  return (
    <nav
      data-slot="nav-tabs"
      aria-label={ariaLabel}
      className={cn("min-w-0 border-b border-border", className)}
      {...props}
    >
      <ul ref={listRef} className="-mb-px flex gap-1 overflow-x-auto">
        {items.map((item) => {
          const active = item.id === activeId;
          const count = item.count && item.count.value > 0 ? item.count : undefined;
          return (
            <li key={item.id} className="shrink-0">
              {renderLink(
                {
                  href: item.href,
                  "aria-current": active ? "page" : undefined,
                  "data-active": active ? "true" : undefined,
                  className: cn(
                    "relative inline-flex h-control items-center gap-2 rounded-t-(--radius-field) px-3 text-sm whitespace-nowrap pointer-coarse:h-control-touch",
                    "transition-colors duration-(--duration-fast)",
                    "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring",
                    "after:absolute after:inset-x-2 after:bottom-0 after:h-0.5 after:rounded-full",
                    active
                      ? "font-medium text-foreground after:bg-primary"
                      : "text-muted-foreground hover:bg-accent-subtle hover:text-foreground hover:after:bg-border",
                  ),
                  children: (
                    <>
                      {renderIcon(item.icon, "size-4 shrink-0")}
                      {item.label}
                      {count ? (
                        <>
                          <span
                            aria-hidden
                            className="rounded-full bg-secondary px-1.5 text-xs font-medium text-secondary-foreground tabular-nums"
                          >
                            {count.value > 99 ? "99+" : count.value}
                          </span>
                          <span className="sr-only">, {count.label}</span>
                        </>
                      ) : null}
                    </>
                  ),
                },
                item,
              )}
            </li>
          );
        })}
      </ul>
    </nav>
  );
}

export { NavTabs };
export type { NavTabsItem, NavTabsLinkProps, NavTabsProps };
