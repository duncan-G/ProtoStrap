using ProtoStrap.Hosting.Yaml;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// The spec is read by ProtoStrap's own parser, which reads a subset of YAML, and by Terraform's
/// yamldecode, so a string is written plain only when neither could read it as anything else.
/// </summary>
public class YamlWriterTests
{
    [Theory]
    [InlineData("c8g.medium")]
    [InlineData("70%")]
    [InlineData("2/s")]
    [InlineData("500m")]
    [InlineData("512Mi")]
    [InlineData("1.4.0")]
    [InlineData("protofast.dev/api/")]
    [InlineData("/healthz")]
    [InlineData("services/api/src/ProtoFast.Api/ProtoFast.Api.csproj")]
    [InlineData("ConnectionStrings__protofast")]
    [InlineData("http://conversion:8090")]
    [InlineData("--rdb")]
    [InlineData("in_memory")]
    [InlineData("no-reply@protofast.dev")]
    public void UnambiguousStringsArePlainInABlock(string value) =>
        Assert.Equal(value, YamlWriter.Scalar(value, inFlow: false));

    [Theory]
    [InlineData("", "\"\"")]
    [InlineData("true", "\"true\"")]
    [InlineData("False", "\"False\"")]
    [InlineData("yes", "\"yes\"")]
    [InlineData("No", "\"No\"")]
    [InlineData("on", "\"on\"")]
    [InlineData("OFF", "\"OFF\"")]
    [InlineData("y", "\"y\"")]
    [InlineData("null", "\"null\"")]
    [InlineData("~", "\"~\"")]
    [InlineData("8080", "\"8080\"")]
    [InlineData("100000", "\"100000\"")]
    [InlineData("-1", "\"-1\"")]
    [InlineData("+1", "\"+1\"")]
    [InlineData("1.5", "\"1.5\"")]
    [InlineData(".5", "\".5\"")]
    [InlineData("1e3", "\"1e3\"")]
    [InlineData("0x1F", "\"0x1F\"")]
    [InlineData("0o17", "\"0o17\"")]
    [InlineData("1_000", "\"1_000\"")]
    [InlineData(".inf", "\".inf\"")]
    [InlineData("12:30", "\"12:30\"")]
    [InlineData("2026-10-25", "\"2026-10-25\"")]
    [InlineData("-", "\"-\"")]
    [InlineData("- x", "\"- x\"")]
    [InlineData("a: b", "\"a: b\"")]
    [InlineData("ends:", "\"ends:\"")]
    [InlineData("x #y", "\"x #y\"")]
    [InlineData("#x", "\"#x\"")]
    [InlineData("{x}", "\"{x}\"")]
    [InlineData("[x]", "\"[x]\"")]
    [InlineData("*x", "\"*x\"")]
    [InlineData("&x", "\"&x\"")]
    [InlineData("!x", "\"!x\"")]
    [InlineData("%x", "\"%x\"")]
    [InlineData("@x", "\"@x\"")]
    [InlineData("`x", "\"`x\"")]
    [InlineData("|", "\"|\"")]
    [InlineData(">x", "\">x\"")]
    [InlineData("?x", "\"?x\"")]
    [InlineData(":(exclude,glob)services/web/**/*.md", "\":(exclude,glob)services/web/**/*.md\"")]
    [InlineData("'x'", "\"'x'\"")]
    [InlineData(" leading", "\" leading\"")]
    [InlineData("trailing ", "\"trailing \"")]
    [InlineData("redis-server --requirepass $REDIS_PASSWORD", "redis-server --requirepass $REDIS_PASSWORD")]
    public void AmbiguousStringsAreQuoted(string value, string expected) =>
        Assert.Equal(expected, YamlWriter.Scalar(value, inFlow: false));

    [Theory]
    [InlineData("say \"hi\"", "say \"hi\"")]
    [InlineData("\"quoted\"", "\"\\\"quoted\\\"\"")]
    [InlineData("a: \\b", "\"a: \\\\b\"")]
    [InlineData("back\\slash", "back\\slash")]
    [InlineData("a\\\"b", "a\\\"b")]
    [InlineData("two\nlines", "\"two\\nlines\"")]
    [InlineData("tab\there", "\"tab\\there\"")]
    [InlineData("bell\u0007", "\"bell\\x07\"")]
    public void QuotesAndBackslashesAreEscapedOnlyWhenQuoted(string value, string expected) =>
        Assert.Equal(expected, YamlWriter.Scalar(value, inFlow: false));

    [Theory]
    [InlineData("a,b", "\"a,b\"")]
    [InlineData("http://conversion:8090", "\"http://conversion:8090\"")]
    [InlineData("x}", "\"x}\"")]
    [InlineData("x]", "\"x]\"")]
    [InlineData("--rdb", "--rdb")]
    [InlineData("c8g.medium", "c8g.medium")]
    public void FlowCollectionsQuoteTheirSeparators(string value, string expected) =>
        Assert.Equal(expected, YamlWriter.Scalar(value, inFlow: true));

    [Fact]
    public void ShortCollectionsAreWrittenInFlowStyle()
    {
        var root = new YamlMap
        {
            ["request"] = new YamlMap { ["cpu"] = "500m", ["memory"] = "512Mi" },
            ["depends_on"] = new YamlSeq("postgres", "redis"),
            ["empty"] = new YamlMap(),
            ["none"] = new YamlSeq(),
            ["port"] = YamlScalar.Number(8080),
            ["fifo"] = YamlScalar.Bool(true),
        };

        Assert.Equal("""
            request: { cpu: 500m, memory: 512Mi }
            depends_on: [postgres, redis]
            empty: {}
            none: []
            port: 8080
            fifo: true

            """, YamlWriter.Write(root));
    }

    [Fact]
    public void BlockCollectionsAreOneEntryPerLine()
    {
        var root = new YamlMap
        {
            ["sources"] = new YamlSeq(YamlStyle.Block, "nuget.config", "services/api"),
            ["env"] = new YamlMap(YamlStyle.Block) { ["A"] = "1x", ["B"] = "true" },
        };

        Assert.Equal("""
            sources:
              - nuget.config
              - services/api
            env:
              A: 1x
              B: "true"

            """, YamlWriter.Write(root));
    }

    [Fact]
    public void AutoCollectionsTooLongForOneLineAreWrittenInBlockStyle()
    {
        var root = new YamlMap
        {
            ["scale"] = new YamlMap
            {
                ["min"] = YamlScalar.Number(0),
                ["max"] = YamlScalar.Number(20),
                ["demand"] = new YamlSeq(new YamlMap
                {
                    ["queue"] = "protofast-document-upload-with-a-name-long-enough-to-wrap",
                    ["rate"] = "2/s",
                    ["drain"] = "1m",
                }),
            },
        };

        Assert.Equal("""
            scale:
              min: 0
              max: 20
              demand: [{ queue: protofast-document-upload-with-a-name-long-enough-to-wrap, rate: 2/s, drain: 1m }]

            """, YamlWriter.Write(root, width: 110));

        Assert.Equal("""
            scale:
              min: 0
              max: 20
              demand:
                - { queue: protofast-document-upload-with-a-name-long-enough-to-wrap, rate: 2/s, drain: 1m }

            """, YamlWriter.Write(root, width: 80));
    }

    [Fact]
    public void EmptyBlockCollectionsAreWrittenInFlowStyle()
    {
        var root = new YamlMap
        {
            ["env"] = new YamlMap(YamlStyle.Block),
            ["sources"] = new YamlSeq(YamlStyle.Block),
        };

        Assert.Equal("""
            env: {}
            sources: []

            """, YamlWriter.Write(root));
    }

    [Fact]
    public void BlankLinesSeparateEntriesWhereAsked()
    {
        var components = new YamlMap(YamlStyle.Block) { BlankLineBetweenEntries = true };
        components["a"] = new YamlMap(YamlStyle.Block) { ["kind"] = "service" };
        components["b"] = new YamlMap(YamlStyle.Block) { ["kind"] = "worker" };
        var root = new YamlMap(YamlStyle.Block) { BlankLineBetweenEntries = true };
        root["app"] = "x";
        root["components"] = components;

        Assert.Equal("""
            app: x

            components:
              a:
                kind: service

              b:
                kind: worker

            """, YamlWriter.Write(root));
    }

    [Fact]
    public void GroupsSeparateTopLevelEntries()
    {
        var root = new YamlMap(YamlStyle.Block);
        root["app"] = "x";
        root["protostrap"] = "1.4.0";
        root.AddGroupBreak();
        root["edge"] = new YamlMap(YamlStyle.Block) { ["request"] = new YamlMap { ["cpu"] = "500m" } };

        Assert.Equal("""
            # one
            # two
            app: x
            protostrap: 1.4.0

            edge:
              request: { cpu: 500m }

            """, YamlWriter.Write(root, header: ["one", "two"]));
    }

    [Fact]
    public void NullValuesAreLeftOut()
    {
        var root = new YamlMap { ["a"] = "one", ["b"] = null, ["c"] = "two" };

        Assert.Equal("a: one\nc: two\n", YamlWriter.Write(root));
    }

    [Fact]
    public void NumbersAreInvariant()
    {
        var root = new YamlMap { ["count"] = YamlScalar.Number(1234567), ["share"] = YamlScalar.Number(0.125) };

        Assert.Equal("count: 1234567\nshare: 0.125\n", YamlWriter.Write(root));
    }
}
