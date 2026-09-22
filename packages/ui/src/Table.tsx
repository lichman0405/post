import type {
  HTMLAttributes,
  ReactNode,
  TableHTMLAttributes,
  TdHTMLAttributes,
  ThHTMLAttributes,
} from "react";

import { WithData } from "./attrs";
import { cx } from "./classes";
import "./ui.css";

/**
 * The one table POST renders (T1101).
 *
 * # What it replaced
 *
 * Three hand-written `<table>`s, two of which were the same stylesheet
 * twice: `.pulls-table` (projects.css:1000) and `.settings-members`
 * (projects.css:368) differ only in the selector, rule for rule —
 * `width/border-collapse/font-size`, then th padding + muted colour +
 * border-bottom, then td padding + hairline, then the last-row rule. The
 * third, `.conflicts-threeway-table`, is the boxed variant: every cell
 * bordered, so a value can be read down a column.
 *
 * Both densities are real and both are kept; what goes away is the
 * per-page spelling of them.
 *
 * # Generic over the row, because a table that is not typed is a table
 *   whose columns are index arithmetic
 *
 * `columns` + `rows` + `rowKey` is the whole API. `rowHeader` promotes a
 * cell to `<th scope="row">` (the three-way table's "Field" column, which
 * is a row label rather than data), and `cellAttrs`/`rowAttrs` carry the
 * `data-*` markers the e2e harnesses select on
 * (`data-member-row`, `data-pull-row`, `data-side`). Nothing about those
 * attributes is inferred: a caller that needs one says so.
 */

export interface TableColumn<Row> {
  /** Stable identity for React and for the column's own `<th>`. */
  key: string;
  /** Column heading. */
  header: ReactNode;
  /** The cell body for one row. */
  render: (row: Row) => ReactNode;
  /** Render this column's body cell as `<th scope="row">`. */
  rowHeader?: boolean;
  /** Attributes for this column's body cells (e.g. `data-side="base"`). */
  cellAttrs?: (row: Row) => WithData<TdHTMLAttributes<HTMLTableCellElement>>;
  /** Attributes for this column's heading cell. */
  headerAttrs?: WithData<ThHTMLAttributes<HTMLTableCellElement>>;
}

export interface TableProps<Row> extends Omit<TableHTMLAttributes<HTMLTableElement>, "children"> {
  columns: Array<TableColumn<Row>>;
  rows: Row[];
  /** Stable key for a row. */
  rowKey: (row: Row) => string;
  /** Attributes for every row (e.g. `data-member-row`). */
  rowAttrs?: (row: Row) => WithData<HTMLAttributes<HTMLTableRowElement>>;
  /** `rows` (default): hairline under each row. `grid`: every cell boxed. */
  borders?: "rows" | "grid";
  /** Accessible description of the table; rendered as a real `<caption>`. */
  caption?: ReactNode;
}

export function Table<Row>({
  columns,
  rows,
  rowKey,
  rowAttrs,
  borders = "rows",
  caption,
  className,
  ...rest
}: TableProps<Row>) {
  return (
    <table
      {...rest}
      className={cx("post-table", borders === "grid" && "post-table-grid", className)}
    >
      {/* A real <caption> for accessibility; no class, because no stylesheet
          draws one and an unbacked className is a dead hook (see
          tests/web-smoke/class-liveness.mjs). */}
      {caption !== undefined ? <caption>{caption}</caption> : null}
      <thead>
        <tr>
          {columns.map((column) => (
            <th key={column.key} scope="col" {...column.headerAttrs}>
              {column.header}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {rows.map((row) => (
          <tr key={rowKey(row)} {...(rowAttrs ? rowAttrs(row) : undefined)}>
            {columns.map((column) =>
              column.rowHeader ? (
                <th key={column.key} scope="row" {...(column.cellAttrs ? column.cellAttrs(row) : undefined)}>
                  {column.render(row)}
                </th>
              ) : (
                <td key={column.key} {...(column.cellAttrs ? column.cellAttrs(row) : undefined)}>
                  {column.render(row)}
                </td>
              ),
            )}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
