import type { TOC } from '@ember/component/template-only';
import type { ServiceRow } from 'koroku-console/utils/services';
import ColorBadge from './color-badge';

/**
 * The name of a service in the service lists, with the colour of a
 * blue/green copy of the public service (ARCHITECTURE 7.2). While a
 * deployment runs both copies, the one receiving the requests says so.
 */
const ServiceName: TOC<{
  Element: HTMLDivElement;
  Args: { row: ServiceRow };
}> = <template>
  <div class="service-name" ...attributes>
    <span class="service-name-text">{{@row.service.name}}</span>
    {{#if @row.color}}
      <ColorBadge
        @color={{@row.color}}
        title="公開サービスの色"
        data-test-service-color={{@row.color}}
      />
      {{#if @row.live}}
        <span
          class="service-name-note"
          data-test-service-live
        >リクエストを受信中</span>
      {{else if @row.standby}}
        <span
          class="service-name-note"
          data-test-service-standby
        >デプロイで並走中</span>
      {{/if}}
    {{/if}}
  </div>
</template>;

export default ServiceName;
