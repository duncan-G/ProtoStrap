using ProtoStrap.Hosting.Spec;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Quantities are written the one way the spec writes them (internal/spec/quantity.go), so the
/// same setting always gives the same line, however it was typed in the AppHost.
/// </summary>
public class QuantityTests
{
    [Theory]
    [InlineData("500m", "500m")]
    [InlineData("2000m", "2000m")]
    [InlineData("2", "2000m")]
    [InlineData("0.5", "500m")]
    [InlineData("1.5", "1500m")]
    [InlineData("0.25", "250m")]
    public void CpuIsWrittenInMillicores(string cpu, string expected) =>
        Assert.Equal(expected, Quantity.Cpu(cpu));

    [Theory]
    [InlineData("")]
    [InlineData("half")]
    [InlineData("-1")]
    [InlineData("0.0001")]
    [InlineData("1e3")]
    [InlineData("500 m")]
    public void CpuThatIsntOneIsRefused(string cpu) => Assert.Null(Quantity.Cpu(cpu));

    [Theory]
    [InlineData("512Mi", "512Mi")]
    [InlineData("1024Mi", "1Gi")]
    [InlineData("1Gi", "1Gi")]
    [InlineData("1.5Gi", "1536Mi")]
    [InlineData("8Gi", "8Gi")]
    [InlineData("1Ti", "1024Gi")]
    [InlineData("2048Ki", "2Mi")]
    public void MemoryIsWrittenInGiOrMi(string memory, string expected) =>
        Assert.Equal(expected, Quantity.Memory(memory));

    [Theory]
    [InlineData("")]
    [InlineData("512")]
    [InlineData("512M")]
    [InlineData("1G")]
    [InlineData("-1Gi")]
    [InlineData("1Ki")]
    public void MemoryThatIsntOneIsRefused(string memory) => Assert.Null(Quantity.Memory(memory));

    [Fact]
    public void ZeroIsAQuantity()
    {
        // A pool's reserve can be zero; a request can't, which the generator checks itself.
        Assert.Equal("0m", Quantity.Cpu("0"));
        Assert.Equal("0Mi", Quantity.Memory("0Gi"));
    }

    [Theory]
    [InlineData("100Gi", "100Gi")]
    [InlineData("1Ti", "1024Gi")]
    public void SizesAreWholeGi(string size, string expected) => Assert.Equal(expected, Quantity.Size(size));

    [Theory]
    [InlineData("1.5Gi")]
    [InlineData("0Gi")]
    [InlineData("100G")]
    [InlineData("512Mi")]
    public void SizesThatArentWholeGiAreRefused(string size) => Assert.Null(Quantity.Size(size));

    [Theory]
    [InlineData(0.70, "70%")]
    [InlineData(0.85, "85%")]
    [InlineData(0.625, "62.5%")]
    [InlineData(1.0, "100%")]
    [InlineData(0.01, "1%")]
    public void PercentagesAreWrittenWithAPercentSign(double fraction, string expected) =>
        Assert.Equal(expected, Quantity.Percent(fraction));

    [Theory]
    [InlineData(0.0)]
    [InlineData(-0.5)]
    [InlineData(1.01)]
    [InlineData(double.NaN)]
    public void PercentagesOutsideZeroToOneAreRefused(double fraction) => Assert.Null(Quantity.Percent(fraction));

    [Theory]
    [InlineData(2.0, "2/s")]
    [InlineData(0.5, "0.5/s")]
    [InlineData(200.0, "200/s")]
    [InlineData(0.125, "0.125/s")]
    public void RatesArePerSecond(double perSecond, string expected) => Assert.Equal(expected, Quantity.Rate(perSecond));

    [Theory]
    [InlineData(0.0)]
    [InlineData(-1.0)]
    [InlineData(double.PositiveInfinity)]
    public void RatesMustBePositive(double perSecond) => Assert.Null(Quantity.Rate(perSecond));

    [Theory]
    [InlineData(30, "30s")]
    [InlineData(60, "1m")]
    [InlineData(90, "90s")]
    [InlineData(120, "2m")]
    [InlineData(300, "5m")]
    [InlineData(3600, "1h")]
    [InlineData(5400, "90m")]
    [InlineData(86400, "1d")]
    [InlineData(345600, "4d")]
    [InlineData(1.5, "1500ms")]
    public void DurationsUseTheLargestWholeUnit(double seconds, string expected) =>
        Assert.Equal(expected, Quantity.Duration(TimeSpan.FromSeconds(seconds)));

    [Fact]
    public void NegativeDurationsAreRefused() => Assert.Null(Quantity.Duration(TimeSpan.FromSeconds(-1)));
}
