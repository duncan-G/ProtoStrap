# .NET telemetry under ProtoStrap

Packages: `OpenTelemetry.Extensions.Hosting`, `OpenTelemetry.Exporter.OpenTelemetryProtocol`,
`OpenTelemetry.Instrumentation.AspNetCore`, `OpenTelemetry.Instrumentation.Http`,
`OpenTelemetry.Instrumentation.Runtime`, and `OpenTelemetry.Instrumentation.GrpcNetClient`
for gRPC clients. Aspire's ServiceDefaults already references most of them. The code below
was built and run against version 1.12.0 of each.

## Aspire ServiceDefaults

The template's `ConfigureOpenTelemetry` sends logs, traces and metrics over OTLP when
`OTEL_EXPORTER_OTLP_ENDPOINT` is set, which both `aspire run` and ProtoStrap do. Under
ProtoStrap, turn the console off **before** adding the OpenTelemetry logger, so each line
leaves once, over OTLP, with its trace ID:

```csharp
public static TBuilder ConfigureOpenTelemetry<TBuilder>(this TBuilder builder) where TBuilder : IHostApplicationBuilder
{
    // Under ProtoStrap, logs go over OTLP alone: a console logger would send each line
    // again through stdout. Clear first, so the OpenTelemetry logger added next stays.
    if (Environment.GetEnvironmentVariable("PROTOSTRAP_REPLICA") is { Length: > 0 })
        builder.Logging.ClearProviders();

    builder.Logging.AddOpenTelemetry(logging =>
    {
        logging.IncludeFormattedMessage = true;
        logging.IncludeScopes = true;
    });

    builder.Services.AddOpenTelemetry()
        .WithMetrics(metrics => metrics
            .AddAspNetCoreInstrumentation()
            .AddHttpClientInstrumentation()
            .AddRuntimeInstrumentation()
            .AddMeter(Telemetry.Name))          // A Meter not added here exports nothing.
        .WithTracing(tracing => tracing
            .AddSource(Telemetry.Name)          // Nor does an ActivitySource.
            .AddAspNetCoreInstrumentation(options =>
                // The spec's ready path is checked continually; those aren't worth a trace.
                options.Filter = context => !context.Request.Path.StartsWithSegments("/healthz"))
            .AddHttpClientInstrumentation());

    // ProtoStrap and `aspire run` set the endpoint; a bare `dotnet run` doesn't.
    if (!string.IsNullOrWhiteSpace(builder.Configuration["OTEL_EXPORTER_OTLP_ENDPOINT"]))
        builder.Services.AddOpenTelemetry().UseOtlpExporter();
    return builder;
}
```

- **Remove any `AddJsonConsole` (or other console logger) that `AddServiceDefaults` adds
  under ProtoStrap.** With the code above, it would send every line a second time.
- `UseOtlpExporter()` reads the endpoint and `http/protobuf` from the variables and adds
  each signal's path (`/v1/logs`, `/v1/traces`, `/v1/metrics`) itself. Don't pass options.
- Run under ProtoStrap, it writes nothing to stdout. A crash's stack trace still reaches
  stderr, and Loki.

## Without Aspire

The same `ConfigureOpenTelemetry`, called first thing in `Program.cs`. To keep JSON lines on
stdout instead, export no logs: drop `builder.Logging.AddOpenTelemetry(...)`, and in place of
`UseOtlpExporter()` add `.AddOtlpExporter()` to the metrics and tracing builders only.

## Custom spans and metrics

One source and one meter per component, named the same, and registered above:

```csharp
public static class Telemetry
{
    public const string Name = "Shop.Orders";
    public static readonly ActivitySource Source = new(Name);
    public static readonly Meter Meter = new(Name);
    public static readonly Counter<long> OrdersPlaced = Meter.CreateCounter<long>("shop.orders.placed", description: "Orders placed");
    public static readonly Histogram<double> ChargeSeconds = Meter.CreateHistogram<double>("shop.charge.duration", unit: "s");
}
```

```csharp
using var activity = Telemetry.Source.StartActivity("place order");   // null when nothing listens
activity?.SetTag("order.id", order.Id);                                // IDs on spans, never on metrics
Telemetry.OrdersPlaced.Add(1, new KeyValuePair<string, object?>("channel", "web"));
```

The Aspire template registers `AddSource(builder.Environment.ApplicationName)` and no meter:
name the source after the assembly, or add your own names, and always add the meter.

## A trace through a queue

Packages: `AWSSDK.SQS`, and `OpenTelemetry.Api` (already referenced through the SDK).
Sending, unless `AddAWSInstrumentation()` is on, which injects for you:

```csharp
var attributes = new Dictionary<string, MessageAttributeValue>();
Propagators.DefaultTextMapPropagator.Inject(
    new PropagationContext(Activity.Current?.Context ?? default, Baggage.Current), attributes,
    static (carrier, key, value) => carrier[key] = new MessageAttributeValue { DataType = "String", StringValue = value });
await sqs.SendMessageAsync(new SendMessageRequest { QueueUrl = queueUrl, MessageBody = body, MessageAttributes = attributes }, token);
```

Receiving, in the worker's loop (protostrap-contract's `references/dotnet.md`):

```csharp
var response = await sqs.ReceiveMessageAsync(new ReceiveMessageRequest
{
    QueueUrl = queueUrl, WaitTimeSeconds = 20, MaxNumberOfMessages = 10,
    MessageAttributeNames = ["All"],   // Without this, SQS returns no attributes.
}, token);
foreach (var message in response.Messages ?? [])
{
    var parent = Propagators.DefaultTextMapPropagator.Extract(default, message.MessageAttributes,
        static (attributes, key) => attributes is not null && attributes.TryGetValue(key, out var value) ? [value.StringValue] : []);
    Baggage.Current = parent.Baggage;
    using var activity = Telemetry.Source.StartActivity("process order", ActivityKind.Consumer, parent.ActivityContext);
    activity?.SetTag("messaging.message.id", message.MessageId);
    // The job, then DeleteMessageAsync.
}
```

Namespaces: `System.Diagnostics`, `OpenTelemetry`, `OpenTelemetry.Context.Propagation`,
`Amazon.SQS.Model`.

## A metric for scaling

A `metric` signal or limit reads Prometheus text from the spec's `metrics` endpoint, not
OTLP. ProtoStrap's `sample/services/dotnet/Api` serves it from a second Kestrel endpoint, an
HTTP/1.1 port of its own, and renders the text itself (`ApiMetrics.cs`). Keep that endpoint
off the app port, and the app's routes off the metrics port.
